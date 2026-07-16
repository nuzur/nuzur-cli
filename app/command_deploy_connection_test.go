package app

import (
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
