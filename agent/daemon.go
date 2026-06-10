// Package agent contains the long-running daemon mode of the nuzur CLI.
//
// The daemon holds a persistent LocalAgentChannel bidi stream open against
// the cloud connection-manager. The server drives reverse RPCs (Ping,
// RunQuery, etc.); the agent runs them locally and streams responses back.
//
// Phase 2 supports Ping + RunQuery against a single hardcoded *sqlx.DB
// opened from NUZUR_AGENT_DSN. Phase 3+ adds the connection registry, OS
// keychain, multiple connections, transactions, row-value transport, and
// auto-install.
package agent

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"strings"
	"time"

	"github.com/nuzur/nuzur-cli/agent/connections"
	"github.com/nuzur/nuzur-cli/cmclient"
	"github.com/nuzur/nuzur-cli/constants"
	"github.com/nuzur/nuzur-cli/files"
	pb "github.com/nuzur/nuzur-cli/protodeps/gen"
)

// DaemonOptions configures one run of the daemon's main loop.
type DaemonOptions struct {
	// ConnectionManagerAddress overrides the cloud endpoint; otherwise the
	// constant / NUZUR_CONNECTION_MANAGER_ADDRESS env var is used.
	ConnectionManagerAddress *string
	DisableTLS               bool

	// LocalDB describes the single hardcoded database the agent answers
	// RunQuery against in phase 2. The `start` command is responsible for
	// resolving these (flag → env → saved file → interactive prompt) and
	// passing them in. Empty DSN is allowed — the daemon will run but
	// every RunQuery returns an error.
	Driver string
	DSN    string
}

const (
	reconnectInitial = time.Second
	reconnectMax     = time.Minute
)

// Run blocks until ctx cancels. It dials the cloud, opens a LocalAgentChannel,
// processes reverse RPCs, and reconnects with exponential backoff on stream
// failure.
func Run(ctx context.Context, opts DaemonOptions) error {
	agentUUID, agentToken, err := loadCredentials()
	if err != nil {
		return fmt.Errorf("agent not paired: %w (run `nuzur agent pair` first)", err)
	}

	cm, err := cmclient.New(cmclient.Params{
		Address:    opts.ConnectionManagerAddress,
		DisableTLS: opts.DisableTLS,
	})
	if err != nil {
		return fmt.Errorf("error building cm client: %w", err)
	}
	defer cm.Close()
	log.Printf("dialing connection-manager at %s (tls=%t)", cm.Address, !opts.DisableTLS)

	registry, err := connections.Load()
	if err != nil {
		log.Printf("warning: could not load connection registry: %v (continuing with fallback DSN only)", err)
		registry = &connections.Registry{}
	}
	log.Printf("loaded %d registered local connection(s)", len(registry.Entries))

	driver := opts.Driver
	if driver == "" {
		driver = "mysql"
	}
	fallback, err := openLocalDB(driver, opts.DSN)
	if err != nil {
		log.Printf("warning: failed to open fallback DSN (%s): %v — registered connections will still work", driver, err)
	}
	if fallback == nil && len(registry.Entries) == 0 {
		log.Printf("warning: no registered connections and no NUZUR_AGENT_DSN; RunQuery requests will fail until you run `nuzur agent connection add` or pass --dsn.")
	}

	pool := newDBPool(registry, fallback)
	defer pool.Close()

	txs := newTxPool()
	defer txs.CloseAll()
	go runIdleTxReaper(ctx, txs)

	backoff := reconnectInitial
	for {
		err := runOnce(ctx, cm, agentUUID, agentToken, pool, txs)
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			log.Printf("daemon stream ended: %v — reconnecting in %s", err, backoff)
		}
		// Whether the stream ended cleanly or with an error, the cloud side
		// has dropped any tx_id state tied to the dead session. Rolling back
		// the agent's open transactions here matches that — otherwise the
		// next session inherits stale tx_ids that the cloud can't reference
		// and the underlying DB connections stay pinned indefinitely.
		txs.CloseAll()
		// On a successful Hello/Welcome (no error), reset backoff so the
		// next failure starts fresh at 1s instead of compounding.
		if err == nil {
			backoff = reconnectInitial
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(backoff):
		}
		backoff = nextBackoff(backoff)
	}
}

func nextBackoff(cur time.Duration) time.Duration {
	next := cur * 2
	if next > reconnectMax {
		next = reconnectMax
	}
	return next
}

// runOnce opens a LocalAgentChannel, says Hello, waits for Welcome, and runs
// the read loop until the stream terminates.
func runOnce(ctx context.Context, cm *cmclient.Client, agentUUID, agentToken string, pool *dbPool, txs *txPool) error {
	streamCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	stream, err := cm.CM.LocalAgentChannel(streamCtx)
	if err != nil {
		return fmt.Errorf("open stream: %w", err)
	}

	// Send Hello.
	if err := stream.Send(&pb.LocalAgentToServer{Message: &pb.LocalAgentToServer_Hello{
		Hello: &pb.Hello{
			LocalAgentToken: agentToken,
			LocalAgentUuid:  agentUUID,
			CliVersion:      constants.CLI_VERSION,
		},
	}}); err != nil {
		return fmt.Errorf("send hello: %w", err)
	}

	// Wait for Welcome.
	first, err := stream.Recv()
	if err != nil {
		return fmt.Errorf("recv welcome: %w", err)
	}
	welcome := first.GetWelcome()
	if welcome == nil {
		return fmt.Errorf("first message from server was not Welcome (got %T)", first.GetMessage())
	}
	log.Printf("paired and online. server_version=%s min_cli=%s", welcome.GetServerVersion(), welcome.GetMinCliVersion())

	// Read loop.
	for {
		msg, err := stream.Recv()
		if err != nil {
			if errors.Is(err, io.EOF) {
				return nil
			}
			return err
		}
		go handleReverseRPC(streamCtx, stream, pool, txs, msg)
	}
}

func handleReverseRPC(ctx context.Context, stream pb.NuzurConnectionManager_LocalAgentChannelClient, pool *dbPool, txs *txPool, msg *pb.ServerToLocalAgent) {
	switch payload := msg.GetMessage().(type) {
	case *pb.ServerToLocalAgent_Ping:
		_ = stream.Send(&pb.LocalAgentToServer{Message: &pb.LocalAgentToServer_Pong{
			Pong: &pb.Pong{RequestId: payload.Ping.GetRequestId()},
		}})

	case *pb.ServerToLocalAgent_RunQuery:
		handleRunQuery(ctx, stream, pool, txs, payload.RunQuery)

	case *pb.ServerToLocalAgent_Exec:
		handleExec(ctx, stream, pool, txs, payload.Exec)

	case *pb.ServerToLocalAgent_BeginTx:
		handleBeginTx(ctx, stream, pool, txs, payload.BeginTx)

	case *pb.ServerToLocalAgent_Commit:
		handleCommit(stream, txs, payload.Commit)

	case *pb.ServerToLocalAgent_Rollback:
		handleRollback(stream, txs, payload.Rollback)

	default:
		log.Printf("unhandled reverse RPC: %T", payload)
	}
}

func sendQueryError(stream pb.NuzurConnectionManager_LocalAgentChannelClient, reqID uint64, msg string) {
	_ = stream.Send(&pb.LocalAgentToServer{Message: &pb.LocalAgentToServer_QueryError{
		QueryError: &pb.QueryError{RequestId: reqID, Message: msg},
	}})
}

// runIdleTxReaper sweeps idle transactions every 15s. The pool's reapIdle
// uses txIdleTimeout (60s) as the cutoff, so an unused tx survives for at
// most ~75s before getting rolled back.
func runIdleTxReaper(ctx context.Context, txs *txPool) {
	t := time.NewTicker(15 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			txs.reapIdle()
		}
	}
}

func loadCredentials() (string, string, error) {
	uuidBytes, err := os.ReadFile(files.LocalAgentUUIDFilePath())
	if err != nil {
		return "", "", err
	}
	tokenBytes, err := os.ReadFile(files.LocalAgentTokenFilePath())
	if err != nil {
		return "", "", err
	}
	// Trim whitespace: text editors and shell redirection sometimes append a
	// trailing newline that would otherwise break the server-side filter match.
	return strings.TrimSpace(string(uuidBytes)), strings.TrimSpace(string(tokenBytes)), nil
}
