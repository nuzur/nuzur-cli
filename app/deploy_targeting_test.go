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

// atID is `at` with an explicit id. A record can now be reusable WITHOUT an agent
// uuid to identify it by — that is precisely what the checkpoint arm decides — so
// the table below expects record ids rather than agents.
func atID(id, host, identifier, agent string, ageMinutes int) deploy.Deployment {
	d := at(host, identifier, agent, "conn", ageMinutes)
	d.ID = id
	return d
}

// steppedTo sets the checkpoint a record's run reached, for the rows whose
// records did not all get equally far.
func steppedTo(d deploy.Deployment, step string) deploy.Deployment {
	d.LastCompletedStep = step
	return d
}

func TestPickPriorDeployment(t *testing.T) {
	for _, tc := range []struct {
		name string
		deps []deploy.Deployment
		// lastCompletedStep is how far the run that wrote these records got — the
		// fact pickPriorDeployment now reads instead of inferring from the agent
		// uuid. "" is a record written before checkpoints existed, which is the
		// arm that must keep behaving exactly as it did.
		//
		// Applied to every record in deps that does not carry one of its own, so
		// the ordinary row reads as one column and the row with MIXED checkpoints
		// can still be written.
		lastCompletedStep string
		host              string
		identifier        string
		wantID            string // "" == expect nil
	}{
		{name: "no records", deps: nil, host: "h1", identifier: "app", wantID: ""},
		{
			name: "exact match",
			deps: []deploy.Deployment{atID("d1", "h1", "app", "agent-1", 0)},
			host: "h1", identifier: "app", wantID: "d1",
		},
		{
			name: "newest of several wins",
			deps: []deploy.Deployment{atID("old", "h1", "app", "a", 0), atID("new", "h1", "app", "a", 60), atID("mid", "h1", "app", "a", 30)},
			host: "h1", identifier: "app", wantID: "new",
		},
		{
			name: "host must match",
			deps: []deploy.Deployment{atID("d1", "h2", "app", "agent-1", 0)},
			host: "h1", identifier: "app", wantID: "",
		},
		{
			name: "identifier must match",
			deps: []deploy.Deployment{atID("d1", "h1", "other", "agent-1", 0)},
			host: "h1", identifier: "app", wantID: "",
		},
		{
			// THE FALLBACK ARM. These records predate checkpoints, so the empty
			// agent uuid is the only evidence there is, and it still means "died
			// before pairing" — unchanged behaviour for every record already on a
			// user's machine.
			name: "a pre-checkpoint record without an agent is skipped",
			deps: []deploy.Deployment{atID("dead", "h1", "app", "", 60), atID("live", "h1", "app", "agent-1", 0)},
			host: "h1", identifier: "app", wantID: "live",
		},
		{
			// THE CHECKPOINT ARM, negative side: the record says it got as far as
			// recording the box and no further, which is a deploy that died in
			// flight. Skipping it is what lets the next run ADOPT the box instead
			// of treating the record as a working prior deployment.
			name:              "a record that died after box_recorded is skipped",
			deps:              []deploy.Deployment{atID("d1", "h1", "app", "", 0)},
			lastCompletedStep: deploy.StepBoxRecorded,
			host:              "h1", identifier: "app", wantID: "",
		},
		{
			// THE CHECKPOINT ARM, positive side. A record cannot normally reach
			// this checkpoint without an agent uuid — the two ride the same write —
			// but if one ever does, the recorded fact is what decides, not the
			// missing field. This is the case the old heuristic got wrong: a uuid
			// dropped by a wholesale write made a paired deployment unreusable.
			name:              "a record that reached agent_paired is reusable without an agent uuid",
			deps:              []deploy.Deployment{atID("d1", "h1", "app", "", 0)},
			lastCompletedStep: deploy.StepAgentPaired,
			host:              "h1", identifier: "app", wantID: "d1",
		},
		{
			name:              "a finalized record is reusable without an agent uuid",
			deps:              []deploy.Deployment{atID("d1", "h1", "app", "", 0)},
			lastCompletedStep: deploy.StepFinalized,
			host:              "h1", identifier: "app", wantID: "d1",
		},
		{
			// Recency only decides among USABLE records. The newest here died in
			// flight, and picking it would hand the deploy a record with no agent
			// and no connection to push a schema through.
			name: "the newest USABLE record wins, not the newest record",
			deps: []deploy.Deployment{
				steppedTo(atID("newer-dead", "h1", "app", "", 60), deploy.StepBoxRecorded),
				steppedTo(atID("older-done", "h1", "app", "agent-1", 0), deploy.StepFinalized),
			},
			host: "h1", identifier: "app", wantID: "older-done",
		},
		{
			// How every record a current CLI writes actually looks: both arms
			// present and agreeing.
			name:              "a finished record with both the uuid and the checkpoint is reusable",
			deps:              []deploy.Deployment{atID("d1", "h1", "app", "agent-1", 0)},
			lastCompletedStep: deploy.StepFinalized,
			host:              "h1", identifier: "app", wantID: "d1",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			deps := append([]deploy.Deployment(nil), tc.deps...)
			for i := range deps {
				if deps[i].LastCompletedStep == "" {
					deps[i].LastCompletedStep = tc.lastCompletedStep
				}
			}
			got := pickPriorDeployment(deps, tc.host, tc.identifier)
			if tc.wantID == "" {
				if got != nil {
					t.Fatalf("expected no match, got record %q (agent %q, step %q)", got.ID, got.LocalAgentUUID, got.LastCompletedStep)
				}
				return
			}
			if got == nil {
				t.Fatalf("expected record %q, got no match", tc.wantID)
			}
			if got.ID != tc.wantID {
				t.Fatalf("record = %q, want %q", got.ID, tc.wantID)
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

// The deployment record is rewritten wholesale as soon as the box exists (step 6b),
// ~20 minutes before step 12 learns the agent from the pairing wait. It used to write
// an EMPTY agent uuid there, so every re-deploy blanked a perfectly well-known agent
// for the middle of its run: `--plan --deployment <id>` failed in that window with a
// false diagnosis ("the deploy that created it did not finish pairing" — it had), and
// an interrupt made the loss permanent, which is how a second record for one box gets
// created and how destroy's isLast then refuses to delete the VM.
func TestKnownAgentUUIDSurvivesTheMidDeployWrite(t *testing.T) {
	prior := at("h1", "app", "agent-prior", "conn-1", 0)
	adopted := at("h1", "app", "agent-adopted", "conn-2", 0)
	agentless := at("h1", "app", "", "conn-3", 0)

	for _, tc := range []struct {
		name     string
		prior    *deploy.Deployment
		boxAgent string
		adopted  *deploy.Deployment
		want     string
	}{
		{
			// The headline case: a managed re-deploy, where both the prior record and
			// the box agree on the agent. It must reach the 6b write.
			name:  "a re-deploy keeps the agent it is reusing",
			prior: &prior, boxAgent: "agent-prior", want: "agent-prior",
		},
		{
			// A second project landing on a box that already has one: no prior record
			// for this identifier, but the box's shared agent is the one this deploy
			// will pair to, so it is known before pairing confirms it.
			name:     "a co-tenant project takes the box's shared agent",
			boxAgent: "agent-box", want: "agent-box",
		},
		{
			// Adopting a box whose own deploy died after pairing but before finishing.
			name:    "an adopted box's agent is used when nothing else knows one",
			adopted: &adopted, want: "agent-adopted",
		},
		{
			name:  "the prior record wins over the box lookup",
			prior: &prior, boxAgent: "agent-box", adopted: &adopted, want: "agent-prior",
		},
		{
			// A genuine first deploy. Empty is correct and load-bearing: the record
			// reads as "died before pairing" until step 12 fills it in, which is what
			// stops a half-built record being reused as a working prior deployment.
			name: "a first deploy still records no agent",
			want: "",
		},
		{
			name:  "an agentless prior record does not mask the box's agent",
			prior: &agentless, boxAgent: "agent-box", want: "agent-box",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := knownAgentUUID(tc.prior, tc.boxAgent, tc.adopted); got != tc.want {
				t.Fatalf("knownAgentUUID = %q, want %q", got, tc.want)
			}
		})
	}
}

// `--plan --deployment <id>` suggested a rerun command that dropped the selector,
// because a real deploy had no use for it. It does now: the record carries the
// project, provider, box and identifier the user never typed, and a suggestion
// missing all four is unrunnable at best and aimed at the wrong database at worst.
func TestApplyDeploymentSelector(t *testing.T) {
	rec := deploy.Deployment{
		ID: "r6box-c3d31228", Provider: deploy.ProviderDigitalOcean,
		Host: "64.225.62.77", User: "root", Port: 22,
		Identifier: "r6box", ProjectUUID: "0ecca0c4", DBEngine: deploy.DBMySQL,
		WorkspaceDir: "/src/nuzur-r6box", Domain: "app.example.com",
	}
	none := func(string) bool { return false }

	t.Run("an unstated field comes from the record", func(t *testing.T) {
		s := &deploySettings{Provider: "ssh", User: "root", Port: 22, DB: "mysql"}
		adopted, err := applyDeploymentSelector(s, &rec, none)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if s.Project != "0ecca0c4" || s.Identifier != "r6box" || s.Provider != "digitalocean" || s.Host != "64.225.62.77" {
			t.Fatalf("targeting = %+v", s)
		}
		if s.Domain != "app.example.com" || s.SourceDir != "/src/nuzur-r6box" {
			t.Errorf("domain/source-dir not adopted: %+v", s)
		}
		// What was taken has to be said: targeting resolved in silence is how a deploy
		// lands somewhere its operator did not expect.
		for _, want := range []string{"project=0ecca0c4", "identifier=r6box", "provider=digitalocean", "host=64.225.62.77"} {
			if !strings.Contains(strings.Join(adopted, " "), want) {
				t.Errorf("adopted = %v, missing %q", adopted, want)
			}
		}
	})

	t.Run("an explicit flag always wins", func(t *testing.T) {
		s := &deploySettings{Provider: "ssh", Host: "10.0.0.1", Identifier: "mine", User: "root", Port: 2222, DB: "postgres"}
		set := map[string]bool{"host": true, "identifier": true, "port": true, "db": true, "provider": true}
		if _, err := applyDeploymentSelector(s, &rec, func(f string) bool { return set[f] }); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if s.Host != "10.0.0.1" || s.Identifier != "mine" || s.Port != 2222 || s.DB != "postgres" || s.Provider != "ssh" {
			t.Fatalf("the record overrode explicit flags: %+v", s)
		}
		// Everything the user did NOT state still comes from the record.
		if s.Project != "0ecca0c4" {
			t.Errorf("project = %q, want it taken from the record", s.Project)
		}
	})

	t.Run("an external database is refused rather than re-hosted", func(t *testing.T) {
		// The record stores THAT the database is external, never its DSN. Adopting the
		// rest and self-hosting a new empty database on the box is the one outcome here
		// that would silently destroy something.
		ext := rec
		ext.ExternalDB = true
		_, err := applyDeploymentSelector(&deploySettings{}, &ext, none)
		if err == nil {
			t.Fatal("expected an error for an external-database record")
		}
		for _, want := range []string{"EXTERNAL database", "--db-dsn", "--connection", "self-host a new, empty database"} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("error %q missing %q", err, want)
			}
		}
		// Supplying one is what makes it legal again.
		if _, err := applyDeploymentSelector(&deploySettings{DBDSN: "user:pass@tcp(h:3306)/db"}, &ext, none); err != nil {
			t.Errorf("a re-supplied DSN should be accepted: %v", err)
		}
	})

	t.Run("no record is a no-op", func(t *testing.T) {
		s := &deploySettings{Provider: "ssh"}
		adopted, err := applyDeploymentSelector(s, nil, none)
		if err != nil || len(adopted) != 0 || s.Provider != "ssh" {
			t.Fatalf("adopted = %v, err = %v, settings = %+v", adopted, err, s)
		}
	})
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

	// The three records above are all PRE-CHECKPOINT: they carry nothing about how
	// far their run got, so every message about them has to keep inferring from
	// empty fields, and keep its exact wording. The three below are the same
	// situations as a CLI that writes checkpoints records them.
	//
	// diedInFlight is the record `died_in_flight_adoption` seeds: the box exists
	// and is reachable, the run that made it died with an error it wrote down.
	diedInFlight := managedRec("terroirconv-90bda381", deploy.ProviderDigitalOcean, "134.122.10.77", proj, "terroirconv", "", 30)
	diedInFlight.LastCompletedStep = deploy.StepBoxRecorded
	diedInFlight.LastError = "remote bootstrap script failed: exit status 1"
	// A healthy, finished deployment — what a re-deploy normally finds.
	finished := managedRec("terroirconv-3fc793ce", deploy.ProviderDigitalOcean, "167.71.167.254", proj, "terroirconv", "agent-1", 0)
	finished.LastCompletedStep = deploy.StepFinalized
	// The create call is what this one died in, and it says so.
	inFlightRecorded := inFlight
	inFlightRecorded.LastCompletedStep = deploy.StepPendingRecorded
	inFlightRecorded.LastError = "creating the digitalocean droplet: context deadline exceeded"

	for _, tc := range []struct {
		name         string
		in           boxDecisionInput
		wantAction   boxAction
		wantHost     string
		wantRecord   string // record id the decision was taken from; "" == none
		wantErr      bool
		wantInMsg    []string
		wantNotInMsg []string
		wantInErr    []string
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
			// It has no checkpoint, so there is nothing recorded to state and the
			// message is the one it always was.
			wantNotInMsg: []string{"The last run of this deployment"},
		},
		{
			// The same adoption, of a record that says how far it got. The decision
			// is identical — the matrix does not read checkpoints — and the message
			// stops leaving the user to work out why a box is being reused with no
			// agent on it.
			name: "an adopted record's checkpoint and error are stated, not inferred",
			in: boxDecisionInput{Provider: deploy.ProviderDigitalOcean, Identifier: "terroirconv",
				ProjectUUID: proj, Deployments: []deploy.Deployment{diedInFlight}},
			wantAction: boxReuseRecorded, wantHost: "134.122.10.77", wantRecord: diedInFlight.ID,
			wantInMsg: []string{
				"Reusing the existing digitalocean server 134.122.10.77",
				"The last run of this deployment died after 'box_recorded': remote bootstrap script failed: exit status 1.",
				"Pass --new-vm to provision a fresh one instead.",
			},
		},
		{
			// A finished deployment did not die, and a routine re-deploy must not be
			// told how its last run went — the fact is only worth stating about a
			// record that is in a bad state.
			name: "a finished record's reuse says nothing about how it ended",
			in: boxDecisionInput{Provider: deploy.ProviderDigitalOcean, Identifier: "terroirconv",
				ProjectUUID: proj, Deployments: []deploy.Deployment{finished}},
			wantAction: boxReuseRecorded, wantHost: "167.71.167.254", wantRecord: finished.ID,
			wantInMsg:    []string{"Reusing the existing digitalocean server 167.71.167.254"},
			wantNotInMsg: []string{"The last run of this deployment", "died after", "finalized"},
		},
		{
			// No host: provisioning past it would leave a VM nothing points at.
			name: "a mid-provision record with no host fails loudly",
			in: boxDecisionInput{Provider: deploy.ProviderDigitalOcean, Identifier: "terroirconv",
				ProjectUUID: proj, Deployments: []deploy.Deployment{live, inFlight}},
			wantAction: boxFail, wantRecord: inFlight.ID, wantErr: true,
		},
		{
			// The billing warning is CONDITIONAL, and has to stay that way. The flag's
			// main use is recovering from a box that no longer answers — the CLI itself
			// suggests --new-vm there — and in that case "already runs it" and "both
			// servers bill" are both false, as is the destroy the message recommends
			// (the provider 404s). This decision never touches the network, so it
			// cannot know which side of the condition it is on; it states the condition.
			name: "--new-vm forces a fresh box and prices it conditionally",
			in: boxDecisionInput{Provider: deploy.ProviderDigitalOcean, NewVM: true, Identifier: "terroirconv",
				ProjectUUID: proj, Deployments: []deploy.Deployment{live}},
			wantAction: boxProvision, wantRecord: live.ID,
			wantInMsg: []string{
				"--new-vm",
				"is already recorded for it",
				"If that server still exists it keeps billing alongside this one",
				"nuzur-cli destroy terroirconv-3fc793ce",
			},
			wantNotInMsg: []string{"already runs it", "Both servers bill"},
		},
		{
			// --new-vm is also the escape from a wedged mid-provision record.
			name: "--new-vm escapes a mid-provision record",
			in: boxDecisionInput{Provider: deploy.ProviderDigitalOcean, NewVM: true, Identifier: "terroirconv",
				ProjectUUID: proj, Deployments: []deploy.Deployment{inFlight}},
			wantAction: boxProvision, wantRecord: inFlight.ID,
			wantInMsg:    []string{"left mid-provision"},
			wantNotInMsg: []string{"The last run of this deployment"},
		},
		{
			// The same escape, over a record that says where it stopped. "may have
			// leaked a VM" is still the inference — the decision has touched no
			// network — and now it comes with the reason the run stopped.
			name: "--new-vm over a checkpointed mid-provision record states what it stopped on",
			in: boxDecisionInput{Provider: deploy.ProviderDigitalOcean, NewVM: true, Identifier: "terroirconv",
				ProjectUUID: proj, Deployments: []deploy.Deployment{inFlightRecorded}},
			wantAction: boxProvision, wantRecord: inFlightRecorded.ID,
			wantInMsg: []string{
				"left mid-provision",
				"The last run of this deployment died after 'pending_recorded': creating the digitalocean droplet: context deadline exceeded.",
				"If that server still exists it keeps billing alongside this one",
			},
		},
		{
			// The refusal, likewise: same arm, same two remedies, plus the record's
			// own account of why there is nothing to reuse.
			name: "a checkpointed mid-provision record fails with what it stopped on",
			in: boxDecisionInput{Provider: deploy.ProviderDigitalOcean, Identifier: "terroirconv",
				ProjectUUID: proj, Deployments: []deploy.Deployment{inFlightRecorded}},
			wantAction: boxFail, wantRecord: inFlightRecorded.ID, wantErr: true,
			wantInErr: []string{
				"was left mid-provision on digitalocean and never recorded a host",
				"The last run of this deployment died after 'pending_recorded': creating the digitalocean droplet: context deadline exceeded.",
				"Run `nuzur-cli destroy terroirconv-37cb6f92`",
				"re-run with --new-vm",
			},
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
			for _, absent := range tc.wantNotInMsg {
				if strings.Contains(got.Message, absent) {
					t.Errorf("message %q asserts %q, which this decision cannot know", got.Message, absent)
				}
			}
			for _, want := range tc.wantInErr {
				if err == nil || !strings.Contains(err.Error(), want) {
					t.Errorf("error %v missing %q", err, want)
				}
			}
		})
	}
}

// lastRunFact is what lets the messages above state how a deployment ended
// instead of guessing it. Its two SILENT cases are the load-bearing ones: a
// record with nothing recorded must produce the wording those messages had
// before checkpoints existed, and a healthy deployment must not be described as
// though something had gone wrong with it.
func TestLastRunFact(t *testing.T) {
	for _, tc := range []struct {
		name string
		rec  *deploy.Deployment
		want string
	}{
		{name: "no record", rec: nil, want: ""},
		{
			name: "a pre-checkpoint record says nothing",
			rec:  &deploy.Deployment{},
			want: "",
		},
		{
			name: "a finished, clean deployment says nothing",
			rec:  &deploy.Deployment{LastCompletedStep: deploy.StepFinalized},
			want: "",
		},
		{
			name: "a checkpoint and an error",
			rec: &deploy.Deployment{
				LastCompletedStep: deploy.StepBoxRecorded,
				LastError:         "remote bootstrap script failed: exit status 1",
			},
			want: "The last run of this deployment died after 'box_recorded': remote bootstrap script failed: exit status 1.",
		},
		{
			// An interrupt: the checkpoints are written as the run goes, the error
			// is written by a deferred hook the signal skips.
			name: "a checkpoint without an error",
			rec:  &deploy.Deployment{LastCompletedStep: deploy.StepInstanceCreated},
			want: "The last run of this deployment died after 'instance_created'.",
		},
		{
			name: "an error before any checkpoint",
			rec:  &deploy.Deployment{LastError: "issuing provisioning token: permission denied"},
			want: "The last run of this deployment failed before it recorded any step: issuing provisioning token: permission denied.",
		},
		{
			// Finalized AND carrying an error: the deployment itself completed and
			// something after it did not. The error is the part worth saying.
			name: "a finished deployment that ended on an error still says so",
			rec: &deploy.Deployment{
				LastCompletedStep: deploy.StepFinalized,
				LastError:         "reading back the front door: connection refused",
			},
			want: "The last run of this deployment died after 'finalized': reading back the front door: connection refused.",
		},
		{
			// A checkpoint written by a NEWER CLI reads as rank 0 (deploy.StepRank),
			// which must not be mistaken for "finished".
			name: "an unrecognised checkpoint is still reported verbatim",
			rec:  &deploy.Deployment{LastCompletedStep: "some_future_step"},
			want: "The last run of this deployment died after 'some_future_step'.",
		},
		{
			// This CLI's own errors are frequently a diagnosis plus the ways
			// forward. All of it inside another sentence is not a sentence; the
			// record keeps the rest.
			name: "a multi-line error is cut to its first line",
			rec: &deploy.Deployment{
				LastCompletedStep: deploy.StepBoxRecorded,
				LastError:         "remote bootstrap script failed\nRun `nuzur-cli destroy sfapi-1` to remove it, or\nre-run to try again.",
			},
			want: "The last run of this deployment died after 'box_recorded': remote bootstrap script failed …",
		},
		{
			name: "an error that already ends in a full stop does not get a second",
			rec: &deploy.Deployment{
				LastCompletedStep: deploy.StepBoxRecorded,
				LastError:         "the box stopped answering.",
			},
			want: "The last run of this deployment died after 'box_recorded': the box stopped answering.",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := lastRunFact(tc.rec); got != tc.want {
				t.Fatalf("lastRunFact = %q, want %q", got, tc.want)
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
