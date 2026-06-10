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

	backoff := reconnectInitial
	for {
		if err := runOnce(ctx, cm, agentUUID, agentToken, pool); err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			log.Printf("daemon stream ended: %v — reconnecting in %s", err, backoff)
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
func runOnce(ctx context.Context, cm *cmclient.Client, agentUUID, agentToken string, pool *dbPool) error {
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
		go handleReverseRPC(streamCtx, stream, pool, msg)
	}
}

func handleReverseRPC(ctx context.Context, stream pb.NuzurConnectionManager_LocalAgentChannelClient, pool *dbPool, msg *pb.ServerToLocalAgent) {
	switch payload := msg.GetMessage().(type) {
	case *pb.ServerToLocalAgent_Ping:
		_ = stream.Send(&pb.LocalAgentToServer{Message: &pb.LocalAgentToServer_Pong{
			Pong: &pb.Pong{RequestId: payload.Ping.GetRequestId()},
		}})

	case *pb.ServerToLocalAgent_RunQuery:
		handleRunQuery(ctx, stream, pool, payload.RunQuery)

	default:
		log.Printf("unhandled reverse RPC: %T", payload)
	}
}

func handleRunQuery(ctx context.Context, stream pb.NuzurConnectionManager_LocalAgentChannelClient, pool *dbPool, req *pb.RunQueryRequest) {
	db, err := pool.Get(req.GetLocalAgentConnectionUuid())
	if err != nil {
		sendQueryError(stream, req.GetRequestId(), err.Error())
		return
	}

	args := make([]interface{}, len(req.GetArgs()))
	for i, a := range req.GetArgs() {
		args[i] = a
	}

	rows, err := db.QueryxContext(ctx, req.GetSql(), args...)
	if err != nil {
		sendQueryError(stream, req.GetRequestId(), err.Error())
		return
	}
	defer rows.Close()

	cols, _ := rows.Columns()
	cts, _ := rows.ColumnTypes()
	columnMetadata := make([]*pb.ColumnMetadata, len(cols))
	for i, c := range cols {
		var dbTypeName, scanHint string
		if i < len(cts) {
			dbTypeName = cts[i].DatabaseTypeName()
			if st := cts[i].ScanType(); st != nil {
				scanHint = st.Kind().String()
			}
		}
		columnMetadata[i] = &pb.ColumnMetadata{
			Name:             c,
			DatabaseTypeName: dbTypeName,
			ScanTypeHint:     scanHint,
		}
	}

	// Phase 2: we only count rows; row payloads stay empty. TestConnection on
	// the server only calls Next() once, so a single chunk reflecting the row
	// count is sufficient for the demo.
	var rowCount int64
	for rows.Next() {
		rowCount++
	}

	_ = stream.Send(&pb.LocalAgentToServer{Message: &pb.LocalAgentToServer_RowsChunk{
		RowsChunk: &pb.RowsChunk{
			RequestId: req.GetRequestId(),
			Columns:   columnMetadata,
			RowCount:  rowCount,
			More:      false,
		},
	}})
}

func sendQueryError(stream pb.NuzurConnectionManager_LocalAgentChannelClient, reqID uint64, msg string) {
	_ = stream.Send(&pb.LocalAgentToServer{Message: &pb.LocalAgentToServer_QueryError{
		QueryError: &pb.QueryError{RequestId: reqID, Message: msg},
	}})
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
