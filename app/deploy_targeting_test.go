package app

import (
	"testing"
	"time"

	"github.com/nuzur/nuzur-cli/deploy"
)

// at builds a deployment record with just the fields targeting cares about.
func at(host, identifier, agent, conn string, ageMinutes int) deploy.Deployment {
	return deploy.Deployment{
		ID:             identifier + "-" + agent,
		Host:           host,
		Identifier:     identifier,
		LocalAgentUUID: agent,
		ConnUUID:       conn,
		CreatedAt:      time.Date(2026, 7, 1, 12, 0, 0, 0, time.UTC).Add(time.Duration(ageMinutes) * time.Minute),
	}
}

func TestPickPriorDeployment(t *testing.T) {
	for _, tc := range []struct {
		name       string
		deps       []deploy.Deployment
		host       string
		identifier string
		wantAgent  string // "" == expect nil
	}{
		{name: "no records", deps: nil, host: "h1", identifier: "app", wantAgent: ""},
		{
			name: "exact match",
			deps: []deploy.Deployment{at("h1", "app", "agent-1", "conn-1", 0)},
			host: "h1", identifier: "app", wantAgent: "agent-1",
		},
		{
			name: "newest of several wins",
			deps: []deploy.Deployment{at("h1", "app", "old", "c", 0), at("h1", "app", "new", "c", 60), at("h1", "app", "mid", "c", 30)},
			host: "h1", identifier: "app", wantAgent: "new",
		},
		{
			name: "host must match",
			deps: []deploy.Deployment{at("h2", "app", "agent-1", "c", 0)},
			host: "h1", identifier: "app", wantAgent: "",
		},
		{
			name: "identifier must match",
			deps: []deploy.Deployment{at("h1", "other", "agent-1", "c", 0)},
			host: "h1", identifier: "app", wantAgent: "",
		},
		{
			// A record without an agent is a deploy that died before pairing: there
			// is nothing to reuse and nothing to push a schema through.
			name: "record without an agent is skipped",
			deps: []deploy.Deployment{at("h1", "app", "", "c", 60), at("h1", "app", "agent-1", "c", 0)},
			host: "h1", identifier: "app", wantAgent: "agent-1",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := pickPriorDeployment(tc.deps, tc.host, tc.identifier)
			if tc.wantAgent == "" {
				if got != nil {
					t.Fatalf("expected no match, got agent %q", got.LocalAgentUUID)
				}
				return
			}
			if got == nil {
				t.Fatalf("expected agent %q, got no match", tc.wantAgent)
			}
			if got.LocalAgentUUID != tc.wantAgent {
				t.Fatalf("agent = %q, want %q", got.LocalAgentUUID, tc.wantAgent)
			}
		})
	}
}

func TestPickBoxAgent(t *testing.T) {
	for _, tc := range []struct {
		name string
		deps []deploy.Deployment
		host string
		want string
	}{
		{name: "no records", deps: nil, host: "h1", want: ""},
		{
			// A box has ONE shared agent, so any project's record on that host answers.
			name: "any project on the host answers",
			deps: []deploy.Deployment{at("h1", "other-project", "agent-1", "c", 0)},
			host: "h1", want: "agent-1",
		},
		{
			name: "newest wins",
			deps: []deploy.Deployment{at("h1", "a", "old", "c", 0), at("h1", "b", "new", "c", 60)},
			host: "h1", want: "new",
		},
		{
			name: "other hosts ignored",
			deps: []deploy.Deployment{at("h2", "a", "agent-2", "c", 0)},
			host: "h1", want: "",
		},
		{
			name: "record without an agent is skipped",
			deps: []deploy.Deployment{at("h1", "a", "", "c", 60)},
			host: "h1", want: "",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := pickBoxAgent(tc.deps, tc.host); got != tc.want {
				t.Fatalf("pickBoxAgent = %q, want %q", got, tc.want)
			}
		})
	}
}

// destroy resolves the agent to revoke the same way: the record's own uuid if it has
// one, else the box's, because a box has exactly one shared agent. The revoke used to
// read only the record's field, so destroying a died-in-flight record (no uuid, which
// is what pickPriorDeployment skips them for) last revoked nothing at all — while the
// closing message said "shared agent revoked" regardless.
func TestDestroyResolvesTheBoxAgent(t *testing.T) {
	// The pair of records the situation always produces: a failed first deploy that
	// never paired, plus the retry that did.
	died := at("h1", "app", "", "conn-dead", 0)
	paired := at("h1", "app", "agent-1", "conn-1", 60)

	for _, tc := range []struct {
		name       string
		destroying deploy.Deployment
		deps       []deploy.Deployment
		want       string
	}{
		{
			name:       "the record carries its own agent",
			destroying: paired, deps: []deploy.Deployment{died, paired}, want: "agent-1",
		},
		{
			// The reported bug: this record has no uuid, but the box does.
			name:       "an empty record falls back to the box's agent",
			destroying: died, deps: []deploy.Deployment{died, paired}, want: "agent-1",
		},
		{
			// Nothing left on this machine knows the agent — the caller warns rather
			// than claiming a revoke.
			name:       "nothing to resolve stays empty",
			destroying: died, deps: []deploy.Deployment{died}, want: "",
		},
		{
			name:       "another host's agent is not borrowed",
			destroying: died, deps: []deploy.Deployment{died, at("h2", "other", "agent-2", "c", 0)}, want: "",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := firstNonEmpty(tc.destroying.LocalAgentUUID, pickBoxAgent(tc.deps, tc.destroying.Host))
			if got != tc.want {
				t.Fatalf("resolved agent = %q, want %q", got, tc.want)
			}
		})
	}
}

// The summary may only claim the revoke that actually happened.
func TestRevokedSuffix(t *testing.T) {
	if got := revokedSuffix(true); got != ", shared agent revoked" {
		t.Fatalf("revokedSuffix(true) = %q", got)
	}
	if got := revokedSuffix(false); got != "" {
		t.Fatalf("revokedSuffix(false) = %q, want no claim", got)
	}
}

func TestDeploySchemaName(t *testing.T) {
	for _, tc := range []struct {
		name       string
		engine     deploy.DBEngine
		dbName     string
		schemaFlag string
		want       string
	}{
		// In MySQL the database IS the schema, and --db-schema is ignored.
		{name: "mysql uses the database name", engine: deploy.DBMySQL, dbName: "acme", want: "acme"},
		{name: "mysql ignores --db-schema", engine: deploy.DBMySQL, dbName: "acme", schemaFlag: "public", want: "acme"},
		{name: "postgres defaults to public", engine: deploy.DBPostgres, dbName: "acme", want: "public"},
		{name: "postgres honors --db-schema", engine: deploy.DBPostgres, dbName: "acme", schemaFlag: "crm", want: "crm"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := deploySchemaName(tc.engine, tc.dbName, tc.schemaFlag); got != tc.want {
				t.Fatalf("deploySchemaName = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestPlanIdentifier(t *testing.T) {
	cfg := map[string]interface{}{"identifier": "from-config"}
	for _, tc := range []struct {
		name        string
		flag        string
		config      map[string]interface{}
		projectName string
		want        string
	}{
		{name: "flag wins", flag: "from-flag", config: cfg, projectName: "Acme CRM", want: "from-flag"},
		{name: "config is next", config: cfg, projectName: "Acme CRM", want: "from-config"},
		{name: "project name is the fallback", projectName: "Acme CRM", want: "acme_crm"},
		{name: "nil config falls through", config: nil, projectName: "Acme CRM", want: "acme_crm"},
		{name: "empty config value falls through", config: map[string]interface{}{"identifier": ""}, projectName: "Acme CRM", want: "acme_crm"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := planIdentifier(tc.flag, tc.config, tc.projectName); got != tc.want {
				t.Fatalf("planIdentifier = %q, want %q", got, tc.want)
			}
		})
	}
}
