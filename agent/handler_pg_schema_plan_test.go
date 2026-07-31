package agent

import (
	"context"
	"os"
	"strings"
	"testing"
)

// The temp-db factory's root database is the one this connection already uses.
// Hardcoding "postgres" is what made schema apply impossible on DigitalOcean
// Managed Postgres, whose maintenance database is called "defaultdb".
func TestPgDatabaseName(t *testing.T) {
	for _, tc := range []struct {
		dsn  string
		want string
	}{
		{"host=127.0.0.1 port=5432 user=doadmin dbname=terroir sslmode=require", "terroir"},
		{"dbname=defaultdb", "defaultdb"},
		{"host=127.0.0.1 dbname=postgres sslmode=disable", "postgres"},
		{"host=127.0.0.1 user=nobody sslmode=disable", ""},
		{"", ""},
	} {
		t.Run(tc.dsn, func(t *testing.T) {
			if got := pgDatabaseName(tc.dsn); got != tc.want {
				t.Fatalf("pgDatabaseName(%q) = %q, want %q", tc.dsn, got, tc.want)
			}
		})
	}
}

// swapPGDatabase and pgDatabaseName have to agree, since the factory compares
// the database it is asked for against the root name to decide whether it is
// looking at a temp database (which needs search_path pinned and the target
// schema created) or at the customer's own.
func TestPgDatabaseNameRoundTripsWithSwapPGDatabase(t *testing.T) {
	const base = "host=127.0.0.1 port=5432 user=doadmin dbname=defaultdb sslmode=require"
	if got := pgDatabaseName(swapPGDatabase(base, "pgschemadiff_tmp_abc")); got != "pgschemadiff_tmp_abc" {
		t.Fatalf("round trip = %q", got)
	}
	if got := pgDatabaseName(swapPGDatabase(base, pgDatabaseName(base))); got != "defaultdb" {
		t.Fatalf("swapping to the root database should be a no-op, got %q", got)
	}
}

// The end-to-end proof of the DigitalOcean case: a Postgres server with no
// database named "postgres" at all. Before the fix the temp-db factory died
// with `database "postgres" does not exist` and no schema could ever be applied.
//
// Run against a throwaway server that mimics DO's layout:
//
//	docker run -d --name pg -e POSTGRES_PASSWORD=secret -e POSTGRES_DB=defaultdb -p 55432:5432 postgres:16
//	docker exec pg psql -U postgres -d defaultdb -c 'DROP DATABASE postgres;'
//	NUZUR_PG_AGENT_TEST_DSN='host=127.0.0.1 port=55432 user=postgres password=secret dbname=defaultdb sslmode=disable' \
//	  go test -run TestComputePgPlanWithoutAPostgresMaintenanceDatabase ./agent/
func TestComputePgPlanWithoutAPostgresMaintenanceDatabase(t *testing.T) {
	dsn := os.Getenv("NUZUR_PG_AGENT_TEST_DSN")
	if dsn == "" {
		t.Skip("set NUZUR_PG_AGENT_TEST_DSN to a Postgres without a 'postgres' database")
	}

	applySQL, err := computePgPlan(context.Background(), dsn, "public",
		"", "CREATE TABLE nuzur_probe (id BIGINT PRIMARY KEY, email VARCHAR(255) NOT NULL);", true)
	if err != nil {
		t.Fatalf("computePgPlan against a server with no 'postgres' database: %v", err)
	}
	if !strings.Contains(applySQL, "nuzur_probe") {
		t.Fatalf("expected a plan creating the probe table, got:\n%s", applySQL)
	}
}

// The capability is what the server gates the new request shape on. If this
// build stops advertising it, the cloud silently keeps sending reconstructed
// DDL and the never-converging migrations come back — with nothing failing.
func TestAgentCapabilitiesAdvertisesLiveSchemaSource(t *testing.T) {
	caps := agentCapabilities()
	for _, c := range caps {
		if c == CapPgLiveSchemaSource {
			return
		}
	}
	t.Fatalf("agentCapabilities() = %v, missing %q", caps, CapPgLiveSchemaSource)
}

// Must match the constant in nuzur-go's sql-connection-manager; the two sides
// never see each other's source, so the literal is the contract.
func TestCapPgLiveSchemaSourceLiteral(t *testing.T) {
	if CapPgLiveSchemaSource != "pg_live_schema_source" {
		t.Fatalf("capability literal changed to %q; the server gates on the old value", CapPgLiveSchemaSource)
	}
}

// The allowlist must stay exactly what the generator emits, and identical to the
// cloud's list — a wider set here means the agent starts proposing to drop
// object classes from the user's own database.
func TestNuzurManagedObjectTypes(t *testing.T) {
	got := nuzurManagedObjectTypes()
	want := []string{"named_schema", "table", "index", "foreign_key_constraint"}

	if len(got) != len(want) {
		t.Fatalf("managed object types = %v, want %v", got, want)
	}
	for i, w := range want {
		if string(got[i]) != w {
			t.Fatalf("managed object types[%d] = %q, want %q", i, got[i], w)
		}
	}
}

// computePgPlan must reach the local database only when asked to. Pointing it at
// an unreachable DSN makes the live branch fail on connect, which is how we tell
// the two branches apart without standing up Postgres: the legacy branch gets as
// far as the temp-db factory instead.
func TestComputePgPlanSelectsTheLiveSourceOnlyWhenAsked(t *testing.T) {
	const unreachableDSN = "host=127.0.0.1 port=1 user=nobody dbname=nobody sslmode=disable connect_timeout=1"

	t.Run("live branch fails opening the local database", func(t *testing.T) {
		_, err := computePgPlan(context.Background(), unreachableDSN, "public", "", "CREATE TABLE foo();", true)
		if err == nil {
			t.Fatal("expected an error against an unreachable database")
		}
		if !strings.Contains(err.Error(), "reaching local database") {
			t.Fatalf("expected the live-source branch to fail reaching the database, got: %v", err)
		}
	})

	t.Run("legacy branch never touches the local database directly", func(t *testing.T) {
		_, err := computePgPlan(context.Background(), unreachableDSN, "public", "CREATE TABLE foo();", "CREATE TABLE foo();", false)
		if err == nil {
			t.Fatal("expected an error against an unreachable database")
		}
		if strings.Contains(err.Error(), "reaching local database") {
			t.Fatalf("legacy branch should not open the live database, got: %v", err)
		}
	})
}
