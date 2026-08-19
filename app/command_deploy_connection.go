package app

import (
	"bufio"
	"fmt"
	"net"
	"net/url"
	"os"
	"strings"
	"time"

	mysqldriver "github.com/go-sql-driver/mysql"
	"github.com/gofrs/uuid"
	nemgen "github.com/nuzur/nem/idl/gen"
	"github.com/nuzur/nuzur-cli/deploy"
	"github.com/nuzur/nuzur-cli/outputtools"
	"github.com/nuzur/nuzur-cli/productclient"
	pb "github.com/nuzur/nuzur-cli/protodeps/gen"
)

// resolveConnectionForDeploy fetches a team connection (with its KMS-held secret)
// and maps it to the DSN parts the external-DB deploy path consumes. It is the
// server-resolved alternative to --db-dsn: the caller only supplies a connection
// uuid, never the plaintext credentials.
//
// The team is the deployed project's team (targets.project.TeamUuid); a
// connection that doesn't belong to that team resolves as not-found.
//
// store is the connection's store uuid — needed by the REMOTE sql-push extension,
// which applies the schema to a team connection directly from nuzur (rather than
// through the box's agent). See applySchema.
func (i *Implementation) resolveConnectionForDeploy(connUUID, teamUUID string) (engine deploy.DBEngine, host, port, user, pass, name, params, store string, err error) {
	authCtx, err := productclient.ClientContext()
	if err != nil {
		return "", "", "", "", "", "", "", "", err
	}
	conn, err := i.productClient.ProductClient.GetConnectionWithSecret(authCtx, &pb.GetConnectionWithSecretRequest{
		ConnectionUuid: connUUID,
		TeamUuid:       teamUUID,
	})
	if err != nil {
		return "", "", "", "", "", "", "", "", fmt.Errorf("connection %s not found in this project's team: %w", connUUID, err)
	}
	engine, host, port, user, pass, name, params, err = connectionToDSNParts(conn)
	if err != nil {
		return "", "", "", "", "", "", "", "", err
	}
	return engine, host, port, user, pass, name, params, conn.GetStoreUuid(), nil
}

// r2EndpointHost is the suffix Cloudflare gives every account's S3-compatible
// R2 endpoint: https://<account_id>.r2.cloudflarestorage.com. Derivable rather
// than stored, so a store that only recorded its account id still resolves.
const r2EndpointHost = ".r2.cloudflarestorage.com"

// r2Region is the region an S3 client must send to R2. R2 is not regional, but
// SigV4 signs over a region, and Cloudflare accepts exactly one value: "auto".
const r2Region = "auto"

// resolveObjectStoreForDeploy fetches a team ObjectStore (with its KMS-held
// key/secret) and returns the five values the bootstrap writes into the app's
// `aws:` config so the generated /upload and /sign endpoints can reach the
// bucket. It mirrors resolveConnectionForDeploy: the caller supplies only the
// object-store uuid, never the plaintext credentials.
//
// Two providers are supported, and they differ only in how the same five values
// are derived. AWS S3 has a real region and needs no endpoint (the SDK derives
// one). Cloudflare R2 is S3-compatible but account-scoped: the region is always
// "auto" and the endpoint is mandatory, either stored on the config or derived
// from the account id. `endpoint` comes back empty for S3 — and an empty
// endpoint is what keeps an S3 deploy's rendered prod.yaml byte-identical to
// what it was before R2 existed (see bootstrap.sh.tmpl).
//
// The team is the deployed project's team; a store that doesn't belong to that
// team resolves as not-found.
func (i *Implementation) resolveObjectStoreForDeploy(storeUUID, teamUUID string) (region, bucket, key, secret, endpoint string, err error) {
	authCtx, err := productclient.ClientContext()
	if err != nil {
		return "", "", "", "", "", err
	}
	store, err := i.productClient.ProductClient.GetObjectStoreWithSecret(authCtx, &pb.GetObjectStoreWithSecretRequest{
		ObjectStoreUuid: storeUUID,
		TeamUuid:        teamUUID,
	})
	if err != nil {
		return "", "", "", "", "", fmt.Errorf("object store %s not found in this project's team: %w", storeUUID, err)
	}
	// Switch on the declared type rather than on which config happens to be
	// populated: the type is what the store IS, and reading a missing config off
	// the oneof-shaped wrapper would otherwise be a nil dereference.
	switch store.GetType() {
	case nemgen.ObjectStoreType_OBJECT_STORE_TYPE_S_3:
		s3 := store.GetTypeConfig().GetS3()
		if s3 == nil {
			return "", "", "", "", "", fmt.Errorf("object store %s has no S3 configuration", storeUUID)
		}
		region, bucket, key, secret = s3.GetRegion(), s3.GetBucket(), s3.GetKey(), s3.GetSecret()
	case nemgen.ObjectStoreType_OBJECT_STORE_TYPE_R_2:
		r2 := store.GetTypeConfig().GetR2()
		if r2 == nil {
			return "", "", "", "", "", fmt.Errorf("object store %s has no R2 configuration", storeUUID)
		}
		region = r2Region
		bucket, key, secret = r2.GetBucket(), r2.GetKey(), r2.GetSecret()
		endpoint = strings.TrimSpace(r2.GetEndpoint())
		if endpoint == "" {
			accountID := strings.TrimSpace(r2.GetAccountId())
			if accountID == "" {
				return "", "", "", "", "", fmt.Errorf("object store %s is an R2 store with neither an endpoint nor an account id — set one of them so the app knows which Cloudflare account to reach", storeUUID)
			}
			endpoint = "https://" + accountID + r2EndpointHost
		}
	default:
		return "", "", "", "", "", fmt.Errorf("object store %s has an unsupported type (%s) — deploy can resolve credentials for S3 and R2 stores only", storeUUID, store.GetType())
	}
	if strings.TrimSpace(bucket) == "" {
		return "", "", "", "", "", fmt.Errorf("object store %s is missing a bucket", storeUUID)
	}
	return region, bucket, key, secret, endpoint, nil
}

// connectionToDSNParts maps a nem Connection into the seven DSN pieces the deploy
// bootstrap needs. Only direct TCP/IP connections are supported (SSH-tunnel
// connections can't be reached from the box the same way). Postgres connections
// carry their database name; MySQL connections are server-level (the database is
// chosen per query), so `name` comes back empty and the caller derives it from
// the deployment identifier.
func connectionToDSNParts(conn *nemgen.Connection) (engine deploy.DBEngine, host, port, user, pass, name, params string, err error) {
	if conn == nil || conn.TypeConfig == nil || conn.TypeConfig.TcpIp == nil {
		return "", "", "", "", "", "", "", fmt.Errorf("connection has no direct TCP/IP configuration (SSH-tunnel connections aren't supported for deploy)")
	}
	tcp := conn.TypeConfig.TcpIp
	host, port, user, pass = tcp.Hostname, tcp.Port, tcp.Username, tcp.Password
	switch conn.DbType {
	case nemgen.ConnectionDbType_CONNECTION_DB_TYPE_POSTGRES:
		engine = deploy.DBPostgres
		if conn.DbTypeConfig == nil || conn.DbTypeConfig.Postgres == nil || conn.DbTypeConfig.Postgres.Database == "" {
			return "", "", "", "", "", "", "", fmt.Errorf("postgres connection is missing a database name")
		}
		name = conn.DbTypeConfig.Postgres.Database
		params = "sslmode=" + pgSSLModeToString(conn.DbTypeConfig.Postgres.Sslmode)
	case nemgen.ConnectionDbType_CONNECTION_DB_TYPE_MYSQL:
		engine = deploy.DBMySQL
		// Merged, not replaced: a stored connection's params are typically just the
		// TLS setting a managed MySQL requires, and taking them wholesale dropped the
		// parseTime the generated app cannot read datetimes without.
		stored := ""
		if conn.DbTypeConfig != nil && conn.DbTypeConfig.Mysql != nil {
			stored = conn.DbTypeConfig.Mysql.Params
		}
		params = mergeDSNParams([]string{"parseTime=true"}, stored)
	default:
		return "", "", "", "", "", "", "", fmt.Errorf("unsupported connection database type")
	}
	if strings.TrimSpace(host) == "" {
		return "", "", "", "", "", "", "", fmt.Errorf("connection is missing a hostname")
	}
	if port == "" {
		if engine == deploy.DBPostgres {
			port = "5432"
		} else {
			port = "3306"
		}
	}
	return engine, host, port, user, pass, name, params, nil
}

// assembleDeployDSN is the inverse of parseDeployDSN: it renders the raw DSN
// string the external-DB bootstrap injects into the on-box agent connection
// (bootstrap.sh.tmpl --dsn / NUZUR_AGENT_DSN). Credentials are escaped so
// passwords with special characters survive the round-trip.
func assembleDeployDSN(engine deploy.DBEngine, host, port, user, pass, name, params string) string {
	if engine == deploy.DBPostgres {
		u := &url.URL{
			Scheme:   "postgres",
			User:     url.UserPassword(user, pass),
			Host:     net.JoinHostPort(host, port),
			Path:     "/" + name,
			RawQuery: params,
		}
		return u.String()
	}
	// MySQL: build via the driver's Config so the password is escaped correctly.
	cfg := mysqldriver.NewConfig()
	cfg.User = user
	cfg.Passwd = pass
	cfg.Net = "tcp"
	cfg.Addr = net.JoinHostPort(host, port)
	cfg.DBName = name
	applyMySQLParams(cfg, params)
	return cfg.FormatDSN()
}

// applyMySQLParams folds a `k=v&k=v` param string onto a driver Config. parseTime
// is a first-class Config field; anything else goes into the generic Params map.
func applyMySQLParams(cfg *mysqldriver.Config, params string) {
	for _, kv := range strings.Split(params, "&") {
		if kv == "" {
			continue
		}
		parts := strings.SplitN(kv, "=", 2)
		key := parts[0]
		val := ""
		if len(parts) == 2 {
			val = parts[1]
		}
		if key == "parseTime" && (val == "true" || val == "1") {
			cfg.ParseTime = true
			continue
		}
		if cfg.Params == nil {
			cfg.Params = map[string]string{}
		}
		cfg.Params[key] = val
	}
}

// mergeDSNParams folds a user-supplied `k=v&k=v` query string ONTO a set of
// defaults instead of replacing them, and returns the merged string.
//
// Replacing was the bug: any `?` on a MySQL DSN overwrote `parseTime=true`, without
// which go-sql-driver hands back a raw []byte for every DATE/DATETIME column, so
// every read of every generated entity fails (`created_at`/`updated_at` are on every
// entity). And a managed MySQL can only be reached WITH a query parameter, because
// TLS is configured as one — so the single thing you must write to connect was also
// the thing that broke the connection. Postgres had the same shape: the
// `sslmode=require` default applied only when the DSN carried no parameters at all,
// so appending any unrelated one silently dropped it.
//
// Defaults keep their order and come first, so an ordinary DSN renders exactly as it
// always did. A user key that names a default REPLACES it in place — passing
// `parseTime=false` or `sslmode=disable` is a deliberate choice and must win —
// and any other key is appended in the order given.
func mergeDSNParams(defaults []string, params string) string {
	// Pairs are carried as written rather than as key/value, so a valueless param
	// (`foo`) survives the round-trip as `foo` and not as `foo=`.
	var order []string
	at := map[string]int{}
	add := func(pair string) {
		if strings.TrimSpace(pair) == "" {
			return
		}
		key, _, _ := strings.Cut(pair, "=")
		if idx, ok := at[key]; ok {
			order[idx] = pair
			return
		}
		at[key] = len(order)
		order = append(order, pair)
	}
	for _, d := range defaults {
		add(d)
	}
	for _, p := range strings.Split(params, "&") {
		add(p)
	}
	return strings.Join(order, "&")
}

// pgSSLModeToString mirrors the connection-manager mapping so a resolved
// connection's sslmode enum renders to the same DSN param value.
func pgSSLModeToString(mode nemgen.DbTypePostgresConfigSslmode) string {
	switch mode {
	case nemgen.DbTypePostgresConfigSslmode_DB_TYPE_POSTGRES_CONFIG_SSLMODE_DISABLE:
		return "disable"
	case nemgen.DbTypePostgresConfigSslmode_DB_TYPE_POSTGRES_CONFIG_SSLMODE_ALLOW:
		return "allow"
	case nemgen.DbTypePostgresConfigSslmode_DB_TYPE_POSTGRES_CONFIG_SSLMODE_PREFER:
		return "prefer"
	case nemgen.DbTypePostgresConfigSslmode_DB_TYPE_POSTGRES_CONFIG_SSLMODE_REQUIRE:
		return "require"
	case nemgen.DbTypePostgresConfigSslmode_DB_TYPE_POSTGRES_CONFIG_SSLMODE_VERIFY_CA:
		return "verify-ca"
	case nemgen.DbTypePostgresConfigSslmode_DB_TYPE_POSTGRES_CONFIG_SSLMODE_VERIFY_FULL:
		return "verify-full"
	}
	return "require"
}

// sslmodeFromParams is the inverse: pull sslmode out of a DSN param string into
// the connection enum. Defaults to REQUIRE (parseDeployDSN's default for pg).
func sslmodeFromParams(params string) nemgen.DbTypePostgresConfigSslmode {
	val := ""
	for _, kv := range strings.Split(params, "&") {
		parts := strings.SplitN(kv, "=", 2)
		if len(parts) == 2 && parts[0] == "sslmode" {
			val = parts[1]
		}
	}
	switch val {
	case "disable":
		return nemgen.DbTypePostgresConfigSslmode_DB_TYPE_POSTGRES_CONFIG_SSLMODE_DISABLE
	case "allow":
		return nemgen.DbTypePostgresConfigSslmode_DB_TYPE_POSTGRES_CONFIG_SSLMODE_ALLOW
	case "prefer":
		return nemgen.DbTypePostgresConfigSslmode_DB_TYPE_POSTGRES_CONFIG_SSLMODE_PREFER
	case "verify-ca":
		return nemgen.DbTypePostgresConfigSslmode_DB_TYPE_POSTGRES_CONFIG_SSLMODE_VERIFY_CA
	case "verify-full":
		return nemgen.DbTypePostgresConfigSslmode_DB_TYPE_POSTGRES_CONFIG_SSLMODE_VERIFY_FULL
	}
	return nemgen.DbTypePostgresConfigSslmode_DB_TYPE_POSTGRES_CONFIG_SSLMODE_REQUIRE
}

// saveConnectionInput carries the resolved external-DB facts needed to register
// the deployed database as a team connection.
type saveConnectionInput struct {
	TeamUUID    string
	ProjectName string
	Identifier  string
	Engine      deploy.DBEngine
	Host        string
	Port        string
	User        string
	Pass        string
	Name        string
	Params      string
}

// shouldSaveTeamConnection decides whether to register the deployed external DB
// as a team connection. It is opt-in and never automatic: an explicit flag wins;
// otherwise, only an interactive terminal prompts (default No). Non-interactive
// runs with no flag never save.
func shouldSaveTeamConnection(noSave, save bool) bool {
	if noSave {
		return false
	}
	if save {
		return true
	}
	if !stdinIsInteractive() {
		return false
	}
	fmt.Fprint(outputtools.Stdout, "\nSave this database as a team connection so your team can use the data manager? [y/N]: ")
	line, err := bufio.NewReader(os.Stdin).ReadString('\n')
	if err != nil {
		return false
	}
	answer := strings.ToLower(strings.TrimSpace(line))
	return answer == "y" || answer == "yes"
}

// stdinIsInteractive reports whether stdin is a terminal (character device), so
// we only prompt when a human can answer.
func stdinIsInteractive() bool {
	fi, err := os.Stdin.Stat()
	if err != nil {
		return false
	}
	return fi.Mode()&os.ModeCharDevice != 0
}

// saveTeamConnection registers the deployed external database as a team
// connection so the whole team can open the data manager on it. Best-effort: any
// failure (not a team admin, Starter connection cap, RPC error) is surfaced as a
// warning — the deploy has already succeeded and must not be failed here.
//
// The write is two steps, mirroring nuzur-web: UpdateTeam persists the connection
// metadata with the password blanked, then CreateConnectionSecret stores the
// password in the team's KMS secret. A store is required (the data-manager path
// enforces store_uuid ∈ team.stores); we reuse an active one or create it.
func (i *Implementation) saveTeamConnection(in saveConnectionInput) {
	authCtx, err := productclient.ClientContext()
	if err != nil {
		outputtools.PrintlnColoredErr("Could not save the team connection: "+err.Error(), outputtools.Yellow)
		return
	}
	team, err := i.productClient.ProductClient.GetTeamForUser(authCtx, &pb.GetTeamForUserRequest{TeamUuid: in.TeamUUID})
	if err != nil {
		outputtools.PrintlnColoredErr("Could not load the team to save the connection: "+err.Error(), outputtools.Yellow)
		return
	}

	envUUID := pickEnvironmentUUID(team)
	if envUUID == "" {
		outputtools.PrintlnColoredErr("Could not save the team connection: no active environment on the team.", outputtools.Yellow)
		return
	}
	storeUUID := reuseOrCreateStore(team, in.ProjectName)

	conn, meta := buildTeamConnection(in, storeUUID, envUUID)
	team.Connections = append(team.Connections, meta)

	// Step 1: persist the connection (+ any new store) as team metadata. The team
	// version is sent unchanged — the server checks it optimistically and stamps a
	// fresh one.
	updateCtx, err := productclient.ClientContext()
	if err != nil {
		outputtools.PrintlnColoredErr("Could not save the team connection: "+err.Error(), outputtools.Yellow)
		return
	}
	if _, err := i.productClient.ProductClient.UpdateTeam(updateCtx, &pb.UpdateTeamRequest{Team: team}); err != nil {
		outputtools.PrintlnColoredErr("Could not save the team connection: "+err.Error(), outputtools.Yellow)
		if strings.Contains(err.Error(), "starter_plan_connection_limit_exceeded") {
			outputtools.PrintlnColoredErr("Your plan allows a single team connection — upgrade to add more.", outputtools.Yellow)
		} else if strings.Contains(err.Error(), "correct role") {
			outputtools.PrintlnColoredErr("Only a team admin can add a team connection.", outputtools.Yellow)
		}
		return
	}

	// Step 2: store the password in the team's KMS secret (keyed by connection uuid).
	secretCtx, err := productclient.ClientContext()
	if err != nil {
		outputtools.PrintlnColoredErr("Team connection saved, but storing its secret failed: "+err.Error(), outputtools.Yellow)
		return
	}
	if _, err := i.productClient.ProductClient.CreateConnectionSecret(secretCtx, &pb.CreateConnectionSecretRequest{
		TeamUuid:   in.TeamUUID,
		Connection: conn,
	}); err != nil {
		outputtools.PrintlnColoredErr("Team connection saved, but storing its secret failed: "+err.Error(), outputtools.Yellow)
		return
	}

	outputtools.PrintlnColored("\nSaved as a team connection — your team can now open the data manager on this database.", outputtools.Green)
	fmt.Fprintf(outputtools.Stdout, "  connection: %s (%s)\n", in.Identifier, conn.Uuid)
	outputtools.PrintlnColoredErr("  Note: the data manager connects to this database directly from nuzur — its host:port must be reachable from the internet.", outputtools.Yellow)
}

// pickEnvironmentUUID returns a usable environment for the connection: the
// critical (prod) one if present, else the first active, else the first.
func pickEnvironmentUUID(team *nemgen.Team) string {
	var firstActive string
	for _, e := range team.Enviorments {
		if e.Status != nemgen.EnviormentStatus_ENVIORMENT_STATUS_ACTIVE {
			continue
		}
		if e.Critical {
			return e.Uuid
		}
		if firstActive == "" {
			firstActive = e.Uuid
		}
	}
	if firstActive != "" {
		return firstActive
	}
	if len(team.Enviorments) > 0 {
		return team.Enviorments[0].Uuid
	}
	return ""
}

// reuseOrCreateStore returns the uuid of an active team store, creating one (and
// appending it to the team so the same UpdateTeam persists it) if none exists.
func reuseOrCreateStore(team *nemgen.Team, projectName string) string {
	for _, s := range team.Stores {
		if s.Status == nemgen.StoreStatus_STORE_STATUS_ACTIVE {
			return s.Uuid
		}
	}
	store := &nemgen.Store{
		Uuid:       uuid.Must(uuid.NewV4()).String(),
		Identifier: firstNonEmpty(projectName, "deployments"),
		Status:     nemgen.StoreStatus_STORE_STATUS_ACTIVE,
	}
	team.Stores = append(team.Stores, store)
	return store.Uuid
}

// buildTeamConnection constructs the connection two ways: `secret` carries the
// password (for CreateConnectionSecret) and `meta` has it blanked (for the team
// metadata written via UpdateTeam). Both share the same minted uuid.
func buildTeamConnection(in saveConnectionInput, storeUUID, envUUID string) (secret *nemgen.Connection, meta *nemgen.Connection) {
	connUUID := uuid.Must(uuid.NewV4()).String()
	version := time.Now().Unix()

	dbType := nemgen.ConnectionDbType_CONNECTION_DB_TYPE_MYSQL
	var dbTypeConfig *nemgen.DbTypeConfig
	if in.Engine == deploy.DBPostgres {
		dbType = nemgen.ConnectionDbType_CONNECTION_DB_TYPE_POSTGRES
		dbTypeConfig = &nemgen.DbTypeConfig{Postgres: &nemgen.DbTypePostgresConfig{
			Database: in.Name,
			Sslmode:  sslmodeFromParams(in.Params),
		}}
	} else {
		dbTypeConfig = &nemgen.DbTypeConfig{Mysql: &nemgen.DbTypeMysqlConfig{Params: in.Params}}
	}

	build := func(password string) *nemgen.Connection {
		return &nemgen.Connection{
			Uuid:           connUUID,
			Version:        version,
			StoreUuid:      storeUUID,
			EnviormentUuid: envUUID,
			Identifier:     in.Identifier,
			DbType:         dbType,
			DbTypeConfig:   dbTypeConfig,
			Type:           nemgen.ConnectionType_CONNECTION_TYPE_TCP_IP,
			Status:         nemgen.ConnectionStatus_CONNECTION_STATUS_ACTIVE,
			TypeConfig: &nemgen.ConnectionTypeConfig{TcpIp: &nemgen.TcpIpConnectionTypeConfig{
				Hostname: in.Host,
				Port:     in.Port,
				Username: in.User,
				Password: password,
			}},
		}
	}
	return build(in.Pass), build("")
}
