package app

import (
	"fmt"
	"strings"
	"testing"

	nemgen "github.com/nuzur/nem/idl/gen"
	"github.com/nuzur/nuzur-cli/deploy"
)

// assembleDeployDSN must be the exact inverse of parseDeployDSN: assembling a DSN
// from parts and re-parsing it yields the same parts, including passwords with
// special characters (which arrive from KMS for --connection deploys).
func TestAssembleDeployDSNRoundTrip(t *testing.T) {
	cases := []struct {
		name   string
		engine deploy.DBEngine
		host   string
		port   string
		user   string
		pass   string
		dbName string
		params string
	}{
		{"postgres_simple", deploy.DBPostgres, "db.example.com", "5432", "app", "secret", "mydb", "sslmode=require"},
		{"postgres_special_chars", deploy.DBPostgres, "10.0.0.5", "5433", "app_user", "p@ss:w/rd?#&", "prod_db", "sslmode=verify-full"},
		{"mysql_simple", deploy.DBMySQL, "db.example.com", "3306", "app", "secret", "mydb", "parseTime=true"},
		{"mysql_special_chars", deploy.DBMySQL, "127.0.0.1", "3307", "app_user", "p@ss:word!#", "prod_db", "parseTime=true"},
		// A managed database is reached WITH extra parameters — on MySQL, TLS is one —
		// so they have to survive the round-trip alongside the defaults, in order.
		{"mysql_with_tls", deploy.DBMySQL, "db.ondigitalocean.com", "25060", "doadmin", "secret", "prod_db", "parseTime=true&tls=skip-verify"},
		{"postgres_with_extra_params", deploy.DBPostgres, "db.ondigitalocean.com", "25060", "doadmin", "secret", "prod_db", "sslmode=require&connect_timeout=10"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dsn := assembleDeployDSN(tc.engine, tc.host, tc.port, tc.user, tc.pass, tc.dbName, tc.params)
			engine, host, port, user, pass, name, params, err := parseDeployDSN(dsn)
			if err != nil {
				t.Fatalf("parseDeployDSN(%q) error: %v", dsn, err)
			}
			if engine != tc.engine {
				t.Errorf("engine = %v, want %v (dsn=%q)", engine, tc.engine, dsn)
			}
			if host != tc.host {
				t.Errorf("host = %q, want %q (dsn=%q)", host, tc.host, dsn)
			}
			if port != tc.port {
				t.Errorf("port = %q, want %q (dsn=%q)", port, tc.port, dsn)
			}
			if user != tc.user {
				t.Errorf("user = %q, want %q (dsn=%q)", user, tc.user, dsn)
			}
			if pass != tc.pass {
				t.Errorf("pass = %q, want %q (dsn=%q)", pass, tc.pass, dsn)
			}
			if name != tc.dbName {
				t.Errorf("name = %q, want %q (dsn=%q)", name, tc.dbName, dsn)
			}
			if params != tc.params {
				t.Errorf("params = %q, want %q (dsn=%q)", params, tc.params, dsn)
			}
		})
	}
}

// parseDeployDSN used to REPLACE its default parameters with whatever query string
// the DSN carried, on both engines. On MySQL that dropped parseTime=true, without
// which go-sql-driver hands back raw []byte for DATE/DATETIME and every read of every
// generated entity fails — and reaching a managed MySQL requires a query parameter,
// because TLS is one. On Postgres it dropped sslmode=require, which a managed
// Postgres refuses the connection without.
func TestParseDeployDSNMergesParams(t *testing.T) {
	for _, tc := range []struct {
		name       string
		dsn        string
		wantEngine deploy.DBEngine
		wantParams string
	}{
		{
			name:       "mysql without params keeps the default",
			dsn:        "app:secret@tcp(127.0.0.1:3306)/shop",
			wantEngine: deploy.DBMySQL, wantParams: "parseTime=true",
		},
		{
			name:       "mysql tls param is added, not substituted",
			dsn:        "doadmin:secret@tcp(db.ondigitalocean.com:25060)/shop?tls=skip-verify",
			wantEngine: deploy.DBMySQL, wantParams: "parseTime=true&tls=skip-verify",
		},
		{
			name:       "mysql several params all survive",
			dsn:        "app:s@tcp(h:3306)/shop?tls=true&timeout=30s&charset=utf8mb4",
			wantEngine: deploy.DBMySQL, wantParams: "parseTime=true&tls=true&timeout=30s&charset=utf8mb4",
		},
		{
			name:       "mysql explicit parseTime is not duplicated",
			dsn:        "app:s@tcp(h:3306)/shop?parseTime=true&tls=true",
			wantEngine: deploy.DBMySQL, wantParams: "parseTime=true&tls=true",
		},
		{
			// A deliberate override has to win: the merge adds a default, it does not
			// impose a policy.
			name:       "mysql explicit parseTime=false wins",
			dsn:        "app:s@tcp(h:3306)/shop?parseTime=false",
			wantEngine: deploy.DBMySQL, wantParams: "parseTime=false",
		},
		{
			name:       "postgres without params keeps the default",
			dsn:        "postgres://app:secret@db.example.com:5432/shop",
			wantEngine: deploy.DBPostgres, wantParams: "sslmode=require",
		},
		{
			name:       "postgres unrelated param no longer drops sslmode",
			dsn:        "postgres://app:secret@db.example.com:25060/shop?connect_timeout=10",
			wantEngine: deploy.DBPostgres, wantParams: "sslmode=require&connect_timeout=10",
		},
		{
			name:       "postgres explicit sslmode wins",
			dsn:        "postgres://app:secret@localhost:5432/shop?sslmode=disable",
			wantEngine: deploy.DBPostgres, wantParams: "sslmode=disable",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			engine, _, _, _, _, _, params, err := parseDeployDSN(tc.dsn)
			if err != nil {
				t.Fatalf("parseDeployDSN(%q): %v", tc.dsn, err)
			}
			if engine != tc.wantEngine {
				t.Errorf("engine = %v, want %v", engine, tc.wantEngine)
			}
			if params != tc.wantParams {
				t.Errorf("params = %q, want %q", params, tc.wantParams)
			}
		})
	}
}

func TestConnectionToDSNParts(t *testing.T) {
	t.Run("postgres", func(t *testing.T) {
		conn := &nemgen.Connection{
			DbType: nemgen.ConnectionDbType_CONNECTION_DB_TYPE_POSTGRES,
			DbTypeConfig: &nemgen.DbTypeConfig{Postgres: &nemgen.DbTypePostgresConfig{
				Database: "prod_db",
				Sslmode:  nemgen.DbTypePostgresConfigSslmode_DB_TYPE_POSTGRES_CONFIG_SSLMODE_REQUIRE,
			}},
			Type: nemgen.ConnectionType_CONNECTION_TYPE_TCP_IP,
			TypeConfig: &nemgen.ConnectionTypeConfig{TcpIp: &nemgen.TcpIpConnectionTypeConfig{
				Hostname: "db.example.com", Port: "5432", Username: "app", Password: "secret",
			}},
		}
		engine, host, port, user, pass, name, params, err := connectionToDSNParts(conn)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if engine != deploy.DBPostgres || host != "db.example.com" || port != "5432" ||
			user != "app" || pass != "secret" || name != "prod_db" || params != "sslmode=require" {
			t.Errorf("got engine=%v host=%q port=%q user=%q pass=%q name=%q params=%q",
				engine, host, port, user, pass, name, params)
		}
	})

	t.Run("mysql_server_level_has_no_db_name", func(t *testing.T) {
		conn := &nemgen.Connection{
			DbType:       nemgen.ConnectionDbType_CONNECTION_DB_TYPE_MYSQL,
			DbTypeConfig: &nemgen.DbTypeConfig{Mysql: &nemgen.DbTypeMysqlConfig{Params: "parseTime=true"}},
			Type:         nemgen.ConnectionType_CONNECTION_TYPE_TCP_IP,
			TypeConfig: &nemgen.ConnectionTypeConfig{TcpIp: &nemgen.TcpIpConnectionTypeConfig{
				Hostname: "db.example.com", Username: "app", Password: "secret",
			}},
		}
		engine, _, port, _, _, name, params, err := connectionToDSNParts(conn)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if engine != deploy.DBMySQL {
			t.Errorf("engine = %v, want MySQL", engine)
		}
		if name != "" {
			t.Errorf("name = %q, want empty (mysql is server-level)", name)
		}
		if port != "3306" { // default filled in
			t.Errorf("port = %q, want default 3306", port)
		}
		if params != "parseTime=true" {
			t.Errorf("params = %q, want parseTime=true", params)
		}
	})

	// A stored connection's params are typically just the TLS setting a managed MySQL
	// needs. Taking them wholesale dropped parseTime, and without it the generated app
	// cannot scan a single DATE/DATETIME column — i.e. every read of every entity.
	t.Run("mysql_stored_params_merge_with_parseTime", func(t *testing.T) {
		for _, tc := range []struct {
			name   string
			stored string
			want   string
		}{
			{name: "none stored", stored: "", want: "parseTime=true"},
			{name: "tls is added, not substituted", stored: "tls=skip-verify", want: "parseTime=true&tls=skip-verify"},
			{name: "already carries parseTime", stored: "parseTime=true&tls=true", want: "parseTime=true&tls=true"},
			{name: "an explicit parseTime=false wins", stored: "parseTime=false", want: "parseTime=false"},
		} {
			t.Run(tc.name, func(t *testing.T) {
				conn := &nemgen.Connection{
					DbType:       nemgen.ConnectionDbType_CONNECTION_DB_TYPE_MYSQL,
					DbTypeConfig: &nemgen.DbTypeConfig{Mysql: &nemgen.DbTypeMysqlConfig{Params: tc.stored}},
					Type:         nemgen.ConnectionType_CONNECTION_TYPE_TCP_IP,
					TypeConfig: &nemgen.ConnectionTypeConfig{TcpIp: &nemgen.TcpIpConnectionTypeConfig{
						Hostname: "db.example.com", Username: "app", Password: "secret",
					}},
				}
				_, _, _, _, _, _, params, err := connectionToDSNParts(conn)
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				if params != tc.want {
					t.Errorf("params = %q, want %q", params, tc.want)
				}
			})
		}
	})

	t.Run("rejects_ssh_only_connection", func(t *testing.T) {
		conn := &nemgen.Connection{
			DbType:     nemgen.ConnectionDbType_CONNECTION_DB_TYPE_POSTGRES,
			Type:       nemgen.ConnectionType_CONNECTION_TYPE_TCP_IP_SSH,
			TypeConfig: &nemgen.ConnectionTypeConfig{TcpIpSsh: &nemgen.TcpIpSshConnectionTypeConfig{}},
		}
		if _, _, _, _, _, _, _, err := connectionToDSNParts(conn); err == nil {
			t.Error("expected an error for an SSH-tunnel connection, got nil")
		}
	})

	t.Run("rejects_postgres_without_database", func(t *testing.T) {
		conn := &nemgen.Connection{
			DbType:     nemgen.ConnectionDbType_CONNECTION_DB_TYPE_POSTGRES,
			Type:       nemgen.ConnectionType_CONNECTION_TYPE_TCP_IP,
			TypeConfig: &nemgen.ConnectionTypeConfig{TcpIp: &nemgen.TcpIpConnectionTypeConfig{Hostname: "h"}},
		}
		if _, _, _, _, _, _, _, err := connectionToDSNParts(conn); err == nil {
			t.Error("expected an error for a postgres connection without a database, got nil")
		}
	})
}

// shouldSaveTeamConnection is opt-in: the explicit flags decide, and with no flag
// a non-interactive run (the test's stdin) never saves.
func TestShouldSaveTeamConnection(t *testing.T) {
	if shouldSaveTeamConnection(true, false) {
		t.Error("--no-save-connection must return false")
	}
	if !shouldSaveTeamConnection(false, true) {
		t.Error("--save-connection must return true")
	}
	if shouldSaveTeamConnection(true, true) {
		t.Error("--no-save-connection must win over --save-connection")
	}
	if shouldSaveTeamConnection(false, false) {
		t.Error("no flag + non-interactive stdin must not save")
	}
}

func TestSslmodeParamsRoundTrip(t *testing.T) {
	modes := []nemgen.DbTypePostgresConfigSslmode{
		nemgen.DbTypePostgresConfigSslmode_DB_TYPE_POSTGRES_CONFIG_SSLMODE_DISABLE,
		nemgen.DbTypePostgresConfigSslmode_DB_TYPE_POSTGRES_CONFIG_SSLMODE_ALLOW,
		nemgen.DbTypePostgresConfigSslmode_DB_TYPE_POSTGRES_CONFIG_SSLMODE_PREFER,
		nemgen.DbTypePostgresConfigSslmode_DB_TYPE_POSTGRES_CONFIG_SSLMODE_REQUIRE,
		nemgen.DbTypePostgresConfigSslmode_DB_TYPE_POSTGRES_CONFIG_SSLMODE_VERIFY_CA,
		nemgen.DbTypePostgresConfigSslmode_DB_TYPE_POSTGRES_CONFIG_SSLMODE_VERIFY_FULL,
	}
	for _, m := range modes {
		if got := sslmodeFromParams("sslmode=" + pgSSLModeToString(m)); got != m {
			t.Errorf("round-trip failed for %v: got %v", m, got)
		}
	}
}

// --- resolveObjectStoreForDeploy ---------------------------------------------
//
// The mapping from a team ObjectStore onto the five values the bootstrap writes
// into the generated app's `aws:` block. Two providers share that block, and
// they disagree about two of the five: R2 has no region (SigV4 still signs over
// one, so Cloudflare mandates the literal "auto") and needs an explicit
// endpoint, which the store may either carry or imply via its account id.
//
// The empty endpoint on the S3 arm is load-bearing, not incidental: it is what
// makes the bootstrap emit no `endpoint:` key, which is what keeps an AWS
// deploy's prod.yaml byte-identical to what it rendered before R2 existed.

const testStoreUUID = "0be8a2f0-0000-0000-0000-0000000000s7"
const testTeamUUID = "0be8a2f0-0000-0000-0000-0000000000tm"

func TestResolveObjectStoreForDeploy(t *testing.T) {
	cases := []struct {
		name  string
		store *nemgen.ObjectStore

		wantRegion   string
		wantBucket   string
		wantKey      string
		wantSecret   string
		wantEndpoint string
	}{
		{
			// AWS S3: everything comes off the S3 config verbatim and the endpoint
			// stays empty (the SDK derives one from the region).
			name: "s3",
			store: &nemgen.ObjectStore{
				Uuid: testStoreUUID,
				Type: nemgen.ObjectStoreType_OBJECT_STORE_TYPE_S_3,
				TypeConfig: &nemgen.ObjectStoreTypeConfig{S3: &nemgen.ObjectStoreS3TypeConfig{
					Region: "us-east-1",
					Bucket: "app-uploads",
					Key:    "AKIAEXAMPLE",
					Secret: "s3-secret",
				}},
			},
			wantRegion:   "us-east-1",
			wantBucket:   "app-uploads",
			wantKey:      "AKIAEXAMPLE",
			wantSecret:   "s3-secret",
			wantEndpoint: "",
		},
		{
			// R2 carrying its own endpoint: it is used verbatim, account id or not.
			// A stored endpoint wins so a store behind a custom domain or a
			// jurisdiction-specific host is not silently rewritten to the default one.
			name: "r2_explicit_endpoint",
			store: &nemgen.ObjectStore{
				Uuid: testStoreUUID,
				Type: nemgen.ObjectStoreType_OBJECT_STORE_TYPE_R_2,
				TypeConfig: &nemgen.ObjectStoreTypeConfig{R2: &nemgen.ObjectStoreR2TypeConfig{
					AccountId: "acct123",
					Bucket:    "r2-uploads",
					Key:       "r2-key",
					Secret:    "r2-secret",
					Endpoint:  "https://files.example.com",
				}},
			},
			wantRegion:   "auto",
			wantBucket:   "r2-uploads",
			wantKey:      "r2-key",
			wantSecret:   "r2-secret",
			wantEndpoint: "https://files.example.com",
		},
		{
			// R2 with only an account id: the default Cloudflare endpoint is derived,
			// so a store that recorded the minimum still deploys.
			name: "r2_derived_endpoint",
			store: &nemgen.ObjectStore{
				Uuid: testStoreUUID,
				Type: nemgen.ObjectStoreType_OBJECT_STORE_TYPE_R_2,
				TypeConfig: &nemgen.ObjectStoreTypeConfig{R2: &nemgen.ObjectStoreR2TypeConfig{
					AccountId: "acct123",
					Bucket:    "r2-uploads",
					Key:       "r2-key",
					Secret:    "r2-secret",
				}},
			},
			wantRegion:   "auto",
			wantBucket:   "r2-uploads",
			wantKey:      "r2-key",
			wantSecret:   "r2-secret",
			wantEndpoint: "https://acct123.r2.cloudflarestorage.com",
		},
		{
			// Whitespace is not an endpoint: a blank-but-present endpoint still falls
			// through to the account id rather than writing an empty `endpoint:`.
			name: "r2_blank_endpoint_falls_back_to_account_id",
			store: &nemgen.ObjectStore{
				Uuid: testStoreUUID,
				Type: nemgen.ObjectStoreType_OBJECT_STORE_TYPE_R_2,
				TypeConfig: &nemgen.ObjectStoreTypeConfig{R2: &nemgen.ObjectStoreR2TypeConfig{
					AccountId: " acct456 ",
					Bucket:    "r2-uploads",
					Key:       "r2-key",
					Secret:    "r2-secret",
					Endpoint:  "   ",
				}},
			},
			wantRegion:   "auto",
			wantBucket:   "r2-uploads",
			wantKey:      "r2-key",
			wantSecret:   "r2-secret",
			wantEndpoint: "https://acct456.r2.cloudflarestorage.com",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			isolateHome(t) // for productclient.ClientContext()
			fake := newFakeProductClient()
			fake.ObjectStore = tc.store
			i := fakeImplementation(fake)

			region, bucket, key, secret, endpoint, err := i.resolveObjectStoreForDeploy(testStoreUUID, testTeamUUID)
			if err != nil {
				t.Fatalf("resolveObjectStoreForDeploy: %v", err)
			}
			if region != tc.wantRegion {
				t.Errorf("region = %q, want %q", region, tc.wantRegion)
			}
			if bucket != tc.wantBucket {
				t.Errorf("bucket = %q, want %q", bucket, tc.wantBucket)
			}
			if key != tc.wantKey {
				t.Errorf("key = %q, want %q", key, tc.wantKey)
			}
			if secret != tc.wantSecret {
				t.Errorf("secret = %q, want %q", secret, tc.wantSecret)
			}
			if endpoint != tc.wantEndpoint {
				t.Errorf("endpoint = %q, want %q", endpoint, tc.wantEndpoint)
			}

			// The store is fetched scoped to the deployed project's team — that scoping
			// IS the authorization check, so it is asserted rather than assumed.
			calls := fake.CallsTo("GetObjectStoreWithSecret")
			if len(calls) != 1 {
				t.Fatalf("GetObjectStoreWithSecret called %d times, want 1", len(calls))
			}
			if got := calls[0].Params["object_store_uuid"]; got != testStoreUUID {
				t.Errorf("object_store_uuid = %q, want %q", got, testStoreUUID)
			}
			if got := calls[0].Params["team_uuid"]; got != testTeamUUID {
				t.Errorf("team_uuid = %q, want %q", got, testTeamUUID)
			}
		})
	}
}

// Every arm that cannot produce a usable `aws:` block must say WHY, and must do
// it as an error — the pre-R2 code read .GetS3() unconditionally, so any store
// that was not an S3 store took a nil dereference instead of a diagnosis.
func TestResolveObjectStoreForDeployErrors(t *testing.T) {
	cases := []struct {
		name  string
		store *nemgen.ObjectStore
		// wantIn are substrings the message must contain, so the test pins the
		// diagnosis rather than the phrasing.
		wantIn []string
	}{
		{
			// R2 with neither an endpoint nor an account id: nothing names the host to
			// talk to, and guessing one would produce an app that 503s at runtime.
			name: "r2_missing_endpoint_and_account_id",
			store: &nemgen.ObjectStore{
				Uuid: testStoreUUID,
				Type: nemgen.ObjectStoreType_OBJECT_STORE_TYPE_R_2,
				TypeConfig: &nemgen.ObjectStoreTypeConfig{R2: &nemgen.ObjectStoreR2TypeConfig{
					Bucket: "r2-uploads",
					Key:    "r2-key",
					Secret: "r2-secret",
				}},
			},
			wantIn: []string{testStoreUUID, "endpoint", "account id"},
		},
		{
			// The zero value of the enum. It is named OBJECT_STORE_TYPE_INVALID, and a
			// store carrying it is exactly that — but it used to be indistinguishable
			// from an S3 store, because nothing read the type at all.
			name: "unknown_type",
			store: &nemgen.ObjectStore{
				Uuid:       testStoreUUID,
				Type:       nemgen.ObjectStoreType_OBJECT_STORE_TYPE_INVALID,
				TypeConfig: &nemgen.ObjectStoreTypeConfig{},
			},
			wantIn: []string{testStoreUUID, "OBJECT_STORE_TYPE_INVALID", "unsupported type"},
		},
		{
			name: "s3_type_without_s3_config",
			store: &nemgen.ObjectStore{
				Uuid:       testStoreUUID,
				Type:       nemgen.ObjectStoreType_OBJECT_STORE_TYPE_S_3,
				TypeConfig: &nemgen.ObjectStoreTypeConfig{},
			},
			wantIn: []string{testStoreUUID, "no S3 configuration"},
		},
		{
			name: "r2_type_without_r2_config",
			store: &nemgen.ObjectStore{
				Uuid:       testStoreUUID,
				Type:       nemgen.ObjectStoreType_OBJECT_STORE_TYPE_R_2,
				TypeConfig: &nemgen.ObjectStoreTypeConfig{},
			},
			wantIn: []string{testStoreUUID, "no R2 configuration"},
		},
		{
			// The bucket guard predates R2 and must still fire for both providers —
			// there is one guard, after the switch, so both arms reach it.
			name: "s3_missing_bucket",
			store: &nemgen.ObjectStore{
				Uuid: testStoreUUID,
				Type: nemgen.ObjectStoreType_OBJECT_STORE_TYPE_S_3,
				TypeConfig: &nemgen.ObjectStoreTypeConfig{S3: &nemgen.ObjectStoreS3TypeConfig{
					Region: "us-east-1",
					Key:    "AKIAEXAMPLE",
					Secret: "s3-secret",
				}},
			},
			wantIn: []string{testStoreUUID, "missing a bucket"},
		},
		{
			name: "r2_missing_bucket",
			store: &nemgen.ObjectStore{
				Uuid: testStoreUUID,
				Type: nemgen.ObjectStoreType_OBJECT_STORE_TYPE_R_2,
				TypeConfig: &nemgen.ObjectStoreTypeConfig{R2: &nemgen.ObjectStoreR2TypeConfig{
					AccountId: "acct123",
					Key:       "r2-key",
					Secret:    "r2-secret",
				}},
			},
			wantIn: []string{testStoreUUID, "missing a bucket"},
		},
		{
			// A store the RPC returned as nil at all: still a diagnosis, not a panic.
			name:   "nil_store",
			store:  nil,
			wantIn: []string{testStoreUUID, "unsupported type"},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			isolateHome(t)
			fake := newFakeProductClient()
			fake.ObjectStore = tc.store
			i := fakeImplementation(fake)

			region, bucket, key, secret, endpoint, err := i.resolveObjectStoreForDeploy(testStoreUUID, testTeamUUID)
			if err == nil {
				t.Fatalf("resolveObjectStoreForDeploy succeeded, want an error (got region=%q bucket=%q endpoint=%q)", region, bucket, endpoint)
			}
			// A failed resolve must hand back nothing at all: a partial result would be
			// written into prod.yaml by a caller that only checked some of it.
			if region != "" || bucket != "" || key != "" || secret != "" || endpoint != "" {
				t.Errorf("failed resolve returned values: region=%q bucket=%q key=%q secret=%q endpoint=%q", region, bucket, key, secret, endpoint)
			}
			for _, want := range tc.wantIn {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("error %q does not mention %q", err.Error(), want)
				}
			}
		})
	}
}

// A store the CLI is not allowed to see resolves as not-found, and the message
// says so in the caller's terms rather than surfacing a bare gRPC status.
func TestResolveObjectStoreForDeployRPCError(t *testing.T) {
	isolateHome(t)
	fake := newFakeProductClient()
	fake.GetObjectStoreWithSecretErr = fmt.Errorf("permission denied")
	i := fakeImplementation(fake)

	_, _, _, _, _, err := i.resolveObjectStoreForDeploy(testStoreUUID, testTeamUUID)
	if err == nil {
		t.Fatal("resolveObjectStoreForDeploy succeeded despite a scripted RPC error")
	}
	if !strings.Contains(err.Error(), "not found in this project's team") || !strings.Contains(err.Error(), "permission denied") {
		t.Errorf("error = %q, want it to name the team scoping and wrap the RPC error", err.Error())
	}
}
