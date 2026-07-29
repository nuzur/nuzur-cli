package agent

import (
	"context"
	"strings"
	"testing"
)

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
