package app

import (
	"errors"
	"strings"
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

// managedRec builds a deployment record for the managed-box decision: a VM some
// provider created for us, for one project + identifier.
func managedRec(id string, provider deploy.Provider, host, project, identifier, agent string, ageMinutes int) deploy.Deployment {
	return deploy.Deployment{
		ID:             id,
		Provider:       provider,
		Host:           host,
		User:           "root",
		Port:           22,
		Identifier:     identifier,
		ProjectUUID:    project,
		LocalAgentUUID: agent,
		// A record with a host has an instance id by then; destroy's only handle.
		ProviderInstanceID:   "i-" + id,
		ProviderResourceName: "nuzur-" + id,
		CreatedAt:            time.Date(2026, 7, 1, 12, 0, 0, 0, time.UTC).Add(time.Duration(ageMinutes) * time.Minute),
	}
}

// The reported bug: three `deploy --provider digitalocean --identifier terroirconv`
// runs produced three droplets, three records, three agents, three databases — and
// not one line of output saying a server had been created rather than reused.
func TestDecideDeployBox(t *testing.T) {
	const proj = "p-1"
	const other = "p-2"
	live := managedRec("terroirconv-3fc793ce", deploy.ProviderDigitalOcean, "167.71.167.254", proj, "terroirconv", "agent-1", 0)
	// A deploy that died after the provider handed out an address but before the
	// agent paired: a real VM, billing, with a half-written record (round-4 bug 20).
	halfBuilt := managedRec("terroirconv-90bda381", deploy.ProviderDigitalOcean, "134.122.10.77", proj, "terroirconv", "", 30)
	// A deploy that died DURING the create call: the name was reserved, the address
	// never came back. Nothing to reuse and nothing to reach.
	inFlight := deploy.Deployment{
		ID: "terroirconv-37cb6f92", Provider: deploy.ProviderDigitalOcean, Provisioning: true,
		ProviderResourceName: "nuzur-terroirconv-37cb6f92",
		Identifier:           "terroirconv", ProjectUUID: proj,
		CreatedAt: time.Date(2026, 7, 1, 13, 0, 0, 0, time.UTC),
	}
	byoSSH := managedRec("terroirconv-ssh", deploy.ProviderSSH, "10.0.0.9", proj, "terroirconv", "agent-9", 5)

	for _, tc := range []struct {
		name       string
		in         boxDecisionInput
		wantAction boxAction
		wantHost   string
		wantRecord string // record id the decision was taken from; "" == none
		wantErr    bool
		wantInMsg  []string
	}{
		{
			// --provider ssh is untouched: the user named the box.
			name: "ssh uses the host it was given",
			in: boxDecisionInput{Provider: deploy.ProviderSSH, HostFlag: "1.2.3.4", Identifier: "terroirconv",
				ProjectUUID: proj, Deployments: []deploy.Deployment{live}},
			wantAction: boxUseGivenHost, wantHost: "1.2.3.4",
		},
		{
			// Multi-project co-tenancy: a DIFFERENT project reaches the same box the
			// only way it ever could — explicit --host. The reuse never interferes.
			name: "a different project reaches the same box over --host",
			in: boxDecisionInput{Provider: deploy.ProviderSSH, HostFlag: "167.71.167.254", Identifier: "otherapp",
				ProjectUUID: other, Deployments: []deploy.Deployment{live}},
			wantAction: boxUseGivenHost, wantHost: "167.71.167.254",
		},
		{
			// First deploy: provision as before, but never in silence.
			name: "no record provisions and says so",
			in: boxDecisionInput{Provider: deploy.ProviderDigitalOcean, Identifier: "terroirconv",
				ProjectUUID: proj, Deployments: nil},
			wantAction: boxProvision,
			wantInMsg:  []string{"Creating a new digitalocean VM", `"terroirconv"`, "will bill"},
		},
		{
			name: "the reported bug: a re-deploy reuses the recorded box",
			in: boxDecisionInput{Provider: deploy.ProviderDigitalOcean, Identifier: "terroirconv",
				ProjectUUID: proj, Deployments: []deploy.Deployment{live}},
			wantAction: boxReuseRecorded, wantHost: "167.71.167.254", wantRecord: live.ID,
			wantInMsg: []string{"Reusing the existing digitalocean server 167.71.167.254", "terroirconv-3fc793ce", "--new-vm"},
		},
		{
			name: "the newest recorded box wins",
			in: boxDecisionInput{Provider: deploy.ProviderDigitalOcean, Identifier: "terroirconv",
				ProjectUUID: proj, Deployments: []deploy.Deployment{live, halfBuilt}},
			wantAction: boxReuseRecorded, wantHost: halfBuilt.Host, wantRecord: halfBuilt.ID,
		},
		{
			// The orphan the retry never adopted. It has a host, so it is reachable in
			// principle; the caller decides by trying, not by whether pairing finished.
			name: "a died-in-flight record with a host is still adopted",
			in: boxDecisionInput{Provider: deploy.ProviderDigitalOcean, Identifier: "terroirconv",
				ProjectUUID: proj, Deployments: []deploy.Deployment{halfBuilt}},
			wantAction: boxReuseRecorded, wantHost: "134.122.10.77", wantRecord: halfBuilt.ID,
		},
		{
			// No host: provisioning past it would leave a VM nothing points at.
			name: "a mid-provision record with no host fails loudly",
			in: boxDecisionInput{Provider: deploy.ProviderDigitalOcean, Identifier: "terroirconv",
				ProjectUUID: proj, Deployments: []deploy.Deployment{live, inFlight}},
			wantAction: boxFail, wantRecord: inFlight.ID, wantErr: true,
		},
		{
			name: "--new-vm forces a fresh box and prices it",
			in: boxDecisionInput{Provider: deploy.ProviderDigitalOcean, NewVM: true, Identifier: "terroirconv",
				ProjectUUID: proj, Deployments: []deploy.Deployment{live}},
			wantAction: boxProvision, wantRecord: live.ID,
			wantInMsg: []string{"--new-vm", "Both servers bill", "nuzur-cli destroy terroirconv-3fc793ce"},
		},
		{
			// --new-vm is also the escape from a wedged mid-provision record.
			name: "--new-vm escapes a mid-provision record",
			in: boxDecisionInput{Provider: deploy.ProviderDigitalOcean, NewVM: true, Identifier: "terroirconv",
				ProjectUUID: proj, Deployments: []deploy.Deployment{inFlight}},
			wantAction: boxProvision, wantRecord: inFlight.ID,
			wantInMsg: []string{"left mid-provision"},
		},
		{
			name: "--new-vm on a first deploy is just a first deploy",
			in: boxDecisionInput{Provider: deploy.ProviderDigitalOcean, NewVM: true, Identifier: "terroirconv",
				ProjectUUID: proj, Deployments: nil},
			wantAction: boxProvision,
			wantInMsg:  []string{"Creating a new digitalocean VM"},
		},
		{
			// Another project's box is never adopted: they would collide on the derived
			// database name and user.
			name: "another project's box is not reused",
			in: boxDecisionInput{Provider: deploy.ProviderDigitalOcean, Identifier: "terroirconv",
				ProjectUUID: other, Deployments: []deploy.Deployment{live}},
			wantAction: boxProvision,
		},
		{
			name: "another identifier's box is not reused",
			in: boxDecisionInput{Provider: deploy.ProviderDigitalOcean, Identifier: "somethingelse",
				ProjectUUID: proj, Deployments: []deploy.Deployment{live}},
			wantAction: boxProvision,
		},
		{
			// A BYO-SSH box is the user's, not something this CLI created.
			name: "a BYO-SSH record is not adopted by a managed deploy",
			in: boxDecisionInput{Provider: deploy.ProviderDigitalOcean, Identifier: "terroirconv",
				ProjectUUID: proj, Deployments: []deploy.Deployment{byoSSH}},
			wantAction: boxProvision,
		},
		{
			// Switching providers means a new box — and the old one keeps billing.
			name: "a record on a different provider provisions with a warning",
			in: boxDecisionInput{Provider: deploy.ProviderHetzner, Identifier: "terroirconv",
				ProjectUUID: proj, Deployments: []deploy.Deployment{live}},
			wantAction: boxProvision, wantRecord: live.ID,
			wantInMsg: []string{"Creating a new hetzner VM", "is on digitalocean", "keeps running and billing"},
		},
		{
			// Records written before ProjectUUID existed still belong to their box.
			name: "a record with no project is still reused",
			in: boxDecisionInput{Provider: deploy.ProviderDigitalOcean, Identifier: "terroirconv",
				ProjectUUID: proj,
				Deployments: []deploy.Deployment{managedRec("legacy", deploy.ProviderDigitalOcean, "5.5.5.5", "", "terroirconv", "agent-x", 0)}},
			wantAction: boxReuseRecorded, wantHost: "5.5.5.5", wantRecord: "legacy",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := decideDeployBox(tc.in)
			if tc.wantErr != (err != nil) {
				t.Fatalf("error = %v, wantErr %v", err, tc.wantErr)
			}
			if got.Action != tc.wantAction {
				t.Fatalf("action = %d, want %d (message: %s / err: %v)", got.Action, tc.wantAction, got.Message, err)
			}
			if got.Host != tc.wantHost {
				t.Errorf("host = %q, want %q", got.Host, tc.wantHost)
			}
			gotRecord := ""
			if got.Record != nil {
				gotRecord = got.Record.ID
			}
			if gotRecord != tc.wantRecord {
				t.Errorf("record = %q, want %q", gotRecord, tc.wantRecord)
			}
			// Every managed decision has to say something: the whole point is that a
			// second VM can no longer appear on the bill unannounced.
			if tc.wantAction != boxUseGivenHost && tc.wantAction != boxFail && got.Message == "" {
				t.Error("a managed decision must explain itself; got no message")
			}
			for _, want := range tc.wantInMsg {
				if !strings.Contains(got.Message, want) {
					t.Errorf("message %q missing %q", got.Message, want)
				}
			}
		})
	}
}

// The reuse carries the RECORDED ssh parameters, so it connects the way the deploy
// that created the box did — a managed run has no --user/--port of its own.
func TestDecideDeployBoxCarriesRecordedSSHParams(t *testing.T) {
	rec := managedRec("app-1", deploy.ProviderDigitalOcean, "1.1.1.1", "p", "app", "a", 0)
	rec.User, rec.Port = "deployer", 2222
	got, err := decideDeployBox(boxDecisionInput{
		Provider: deploy.ProviderDigitalOcean, Identifier: "app", ProjectUUID: "p",
		Deployments: []deploy.Deployment{rec},
	})
	if err != nil {
		t.Fatalf("decideDeployBox: %v", err)
	}
	if got.User != "deployer" || got.Port != 2222 {
		t.Fatalf("ssh params = %s:%d, want deployer:2222", got.User, got.Port)
	}
}

// An unreachable reused box must not fall back to provisioning — that is the
// behaviour the reuse exists to remove — so the message has to hand back all three
// ways forward.
func TestReusedBoxUnreachableError(t *testing.T) {
	rec := managedRec("terroirconv-3fc793ce", deploy.ProviderDigitalOcean, "167.71.167.254", "p", "terroirconv", "a", 0)
	err := reusedBoxUnreachableError(&rec, deploy.ProviderDigitalOcean, "terroirconv",
		errors.New("ssh preflight to root@167.71.167.254 failed"))
	for _, want := range []string{
		"not reachable over SSH",
		"ssh preflight to root@167.71.167.254 failed",
		"restore connectivity to 167.71.167.254",
		"nuzur-cli destroy terroirconv-3fc793ce",
		"--new-vm",
	} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("unreachable message missing %q:\n%s", want, err.Error())
		}
	}
}
