package app

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
	"time"

	nemgen "github.com/nuzur/nem/idl/gen"
	"github.com/nuzur/nuzur-cli/deploy"
	"github.com/nuzur/nuzur-cli/sqlplan"
)

// planDep builds a complete, plannable deployment record.
func planDep(id, host, identifier string) deploy.Deployment {
	return deploy.Deployment{
		ID:             id,
		Host:           host,
		Identifier:     identifier,
		ProjectUUID:    "project-1",
		LocalAgentUUID: "agent-1",
		ConnUUID:       "conn-1",
		DBEngine:       deploy.DBPostgres,
		CreatedAt:      time.Date(2026, 7, 1, 12, 0, 0, 0, time.UTC),
	}
}

func TestResolvePlanTargetFromState(t *testing.T) {
	t.Run("explicit agent and connection win over everything", func(t *testing.T) {
		got, err := resolvePlanTargetFromState(planTargetInput{
			LocalAgent:     "flag-agent",
			LocalAgentConn: "flag-conn",
			DeploymentID:   "dep-1",
			Host:           "h1",
			Identifier:     "app",
			DB:             "postgres",
			Deployments:    []deploy.Deployment{planDep("dep-1", "h1", "app")},
		})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if got.AgentUUID != "flag-agent" || got.ConnUUID != "flag-conn" {
			t.Fatalf("got agent=%q conn=%q", got.AgentUUID, got.ConnUUID)
		}
		if got.Mode != connModeLocal {
			t.Fatalf("mode = %q, want local", got.Mode)
		}
	})

	t.Run("half the agent pair is an error naming the missing half", func(t *testing.T) {
		_, err := resolvePlanTargetFromState(planTargetInput{LocalAgent: "a"})
		if err == nil || !strings.Contains(err.Error(), "--local-agent-connection") {
			t.Fatalf("err = %v, want it to name --local-agent-connection", err)
		}
		_, err = resolvePlanTargetFromState(planTargetInput{LocalAgentConn: "c"})
		if err == nil || !strings.Contains(err.Error(), "--local-agent") {
			t.Fatalf("err = %v, want it to name --local-agent", err)
		}
	})

	t.Run("deployment by id", func(t *testing.T) {
		got, err := resolvePlanTargetFromState(planTargetInput{
			DeploymentID: "dep-1",
			ProjectUUID:  "project-1",
			Deployments:  []deploy.Deployment{planDep("dep-1", "h1", "app")},
		})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if got.DeploymentID != "dep-1" || got.AgentUUID != "agent-1" || got.ConnUUID != "conn-1" {
			t.Fatalf("got %+v", got)
		}
		// The record's engine decides the schema, not a flag default.
		if got.Engine != deploy.DBPostgres || got.Schema != "public" {
			t.Fatalf("engine = %q, schema = %q", got.Engine, got.Schema)
		}
		if !strings.Contains(got.Source, "dep-1") {
			t.Fatalf("Source = %q, should name the deployment", got.Source)
		}
	})

	t.Run("unknown deployment id points at deploy list", func(t *testing.T) {
		_, err := resolvePlanTargetFromState(planTargetInput{DeploymentID: "nope"})
		if err == nil || !strings.Contains(err.Error(), "deploy list") {
			t.Fatalf("err = %v", err)
		}
	})

	t.Run("a deployment belonging to another project is refused", func(t *testing.T) {
		dep := planDep("dep-1", "h1", "app")
		dep.ProjectUUID = "someone-elses-project"
		_, err := resolvePlanTargetFromState(planTargetInput{
			DeploymentID: "dep-1",
			ProjectUUID:  "project-1",
			Deployments:  []deploy.Deployment{dep},
		})
		if err == nil {
			t.Fatal("expected an error")
		}
		// Both uuids must appear, or the user cannot tell which is which.
		for _, want := range []string{"someone-elses-project", "project-1"} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("err = %v, missing %q", err, want)
			}
		}
	})

	t.Run("a deployment with no agent explains the alternatives", func(t *testing.T) {
		dep := planDep("dep-1", "h1", "app")
		dep.LocalAgentUUID = ""
		_, err := resolvePlanTargetFromState(planTargetInput{
			DeploymentID: "dep-1", ProjectUUID: "project-1",
			Deployments: []deploy.Deployment{dep},
		})
		if err == nil || !strings.Contains(err.Error(), "--local-agent") {
			t.Fatalf("err = %v, want it to offer the escape hatch", err)
		}
	})

	t.Run("a deployment with no connection explains the alternative", func(t *testing.T) {
		dep := planDep("dep-1", "h1", "app")
		dep.ConnUUID = ""
		_, err := resolvePlanTargetFromState(planTargetInput{
			DeploymentID: "dep-1", ProjectUUID: "project-1",
			Deployments: []deploy.Deployment{dep},
		})
		if err == nil || !strings.Contains(err.Error(), "--local-agent-connection") {
			t.Fatalf("err = %v", err)
		}
	})

	t.Run("a team connection is a remote target", func(t *testing.T) {
		got, err := resolvePlanTargetFromState(planTargetInput{
			TeamConnUUID: "team-conn-1",
			Identifier:   "app",
			DB:           "mysql",
		})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if got.Mode != connModeRemote || got.TeamConnUUID != "team-conn-1" {
			t.Fatalf("got %+v", got)
		}
	})

	t.Run("host plus identifier finds the box a deploy would push to", func(t *testing.T) {
		got, err := resolvePlanTargetFromState(planTargetInput{
			Host:        "h1",
			Identifier:  "app",
			ProjectUUID: "project-1",
			Deployments: []deploy.Deployment{planDep("dep-1", "h1", "app")},
		})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if got.DeploymentID != "dep-1" {
			t.Fatalf("got %+v", got)
		}
	})

	t.Run("no live target at all is not an error", func(t *testing.T) {
		// This is a first deploy. The caller renders the CREATE script instead.
		got, err := resolvePlanTargetFromState(planTargetInput{Host: "brand-new", Identifier: "app"})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if got != nil {
			t.Fatalf("expected no target, got %+v", got)
		}
	})

	t.Run("a raw DSN with nothing else explains why it cannot be reached", func(t *testing.T) {
		_, err := resolvePlanTargetFromState(planTargetInput{
			DBDSN:      "user:pass@tcp(db.example.com:3306)/acme",
			Identifier: "app",
		})
		if err == nil {
			t.Fatal("expected an error")
		}
		// It has to offer all three ways out, since the user did name a database.
		for _, want := range []string{"--deployment", "--local-agent", "--connection"} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("err = %v, missing %q", err, want)
			}
		}
	})

	t.Run("a raw DSN with a prior deployment is fine", func(t *testing.T) {
		// The drift case: an external database deployed once from this machine, so
		// the agent that can reach it is on record.
		got, err := resolvePlanTargetFromState(planTargetInput{
			DBDSN:       "user:pass@tcp(db.example.com:3306)/acme",
			Host:        "h1",
			Identifier:  "app",
			ProjectUUID: "project-1",
			Deployments: []deploy.Deployment{planDep("dep-1", "h1", "app")},
		})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if got == nil || got.DeploymentID != "dep-1" {
			t.Fatalf("got %+v", got)
		}
	})
}

func TestRerunCommand(t *testing.T) {
	for _, tc := range []struct {
		name     string
		argv     []string
		addAllow bool
		want     string
	}{
		{name: "empty argv", argv: nil, want: "nuzur-cli deploy"},
		{
			name: "plan-only flags are dropped",
			argv: []string{"nuzur-cli", "deploy", "--plan", "--json", "--host", "prod"},
			want: "nuzur-cli deploy --host prod",
		},
		{
			// --deployment is the selector that made the plan resolvable, and it works
			// on a real deploy too: dropping it produced a command that targeted
			// nothing (see the doc comment on rerunCommand).
			name: "the deployment selector survives, separated",
			argv: []string{"nuzur-cli", "deploy", "--plan", "--deployment", "acme-3f2a1c", "--version", "v_8"},
			want: "nuzur-cli deploy --deployment acme-3f2a1c --version v_8",
		},
		{
			name: "the deployment selector survives, inline",
			argv: []string{"nuzur-cli", "deploy", "--plan", "--deployment=acme-3f2a1c", "--version", "v_8"},
			want: "nuzur-cli deploy --deployment=acme-3f2a1c --version v_8",
		},
		{
			// The exact round-6 invocation, and the whole point of the fix: what this
			// used to suggest was `nuzur-cli deploy --version 63fc3f92 --allow-destructive`,
			// which has no project, no provider and no identifier — it fails with
			// "--host is required for the ssh provider", or on a TTY prompts for a
			// project and aims --allow-destructive somewhere nobody planned.
			name:     "a destructive plan is pasteable and still targets the deployment",
			argv:     []string{"nuzur-cli", "deploy", "--plan", "--deployment", "r6box-c3d31228", "--version", "63fc3f92"},
			addAllow: true,
			want:     "nuzur-cli deploy --deployment r6box-c3d31228 --version 63fc3f92 --allow-destructive",
		},
		{
			// --local-agent/--local-agent-connection stay dropped: they name a database
			// directly, and a deploy reaches its database through the box instead.
			// The value has to go with the flag, or it lands as a stray argument.
			name: "a plan-only selector drops its separated value too",
			argv: []string{"nuzur-cli", "deploy", "--plan", "--local-agent", "agent-1", "--host", "prod"},
			want: "nuzur-cli deploy --host prod",
		},
		{
			name: "a plan-only selector drops its inline value too",
			argv: []string{"nuzur-cli", "deploy", "--plan", "--local-agent-connection=conn-1", "--host", "prod"},
			want: "nuzur-cli deploy --host prod",
		},
		{
			name:     "--allow-destructive is appended when needed",
			argv:     []string{"nuzur-cli", "deploy", "--plan", "--host", "prod", "--db-only"},
			addAllow: true,
			want:     "nuzur-cli deploy --host prod --db-only --allow-destructive",
		},
		{
			name:     "--allow-destructive is not doubled",
			argv:     []string{"nuzur-cli", "deploy", "--host", "prod", "--allow-destructive"},
			addAllow: true,
			want:     "nuzur-cli deploy --host prod --allow-destructive",
		},
		{
			name: "an absolute invocation path is shortened",
			argv: []string{"/usr/local/bin/nuzur-cli", "deploy", "--host", "prod"},
			want: "nuzur-cli deploy --host prod",
		},
		{
			name: "an argument with spaces is quoted so it stays pasteable",
			argv: []string{"nuzur-cli", "deploy", "--project", "Acme CRM"},
			want: "nuzur-cli deploy --project 'Acme CRM'",
		},
		{
			name: "everything else is preserved in order",
			argv: []string{"nuzur-cli", "deploy", "--provider", "ssh", "--host", "prod", "--db-only", "--version", "v_8"},
			want: "nuzur-cli deploy --provider ssh --host prod --db-only --version v_8",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := rerunCommand(tc.argv, tc.addAllow); got != tc.want {
				t.Fatalf("rerunCommand() = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestReviewStatusName(t *testing.T) {
	for _, tc := range []struct {
		in   nemgen.ProjectVersionReviewStatus
		want string
	}{
		{nemgen.ProjectVersionReviewStatus_PROJECT_VERSION_REVIEW_STATUS_DRAFT, "DRAFT"},
		{nemgen.ProjectVersionReviewStatus_PROJECT_VERSION_REVIEW_STATUS_IN_REVIEW, "IN_REVIEW"},
		{nemgen.ProjectVersionReviewStatus_PROJECT_VERSION_REVIEW_STATUS_APPROVED, "APPROVED"},
		{nemgen.ProjectVersionReviewStatus_PROJECT_VERSION_REVIEW_STATUS_REJECTED, "REJECTED"},
		{nemgen.ProjectVersionReviewStatus_PROJECT_VERSION_REVIEW_STATUS_PUBLISHED, "PUBLISHED"},
	} {
		if got := reviewStatusName(tc.in); got != tc.want {
			t.Errorf("reviewStatusName(%v) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestPlanProjectVersionApproval(t *testing.T) {
	// Only approved and published count as deployable; every other status makes a
	// plan informative but not what a deploy would do.
	for _, tc := range []struct {
		status       nemgen.ProjectVersionReviewStatus
		wantApproved bool
	}{
		{nemgen.ProjectVersionReviewStatus_PROJECT_VERSION_REVIEW_STATUS_APPROVED, true},
		{nemgen.ProjectVersionReviewStatus_PROJECT_VERSION_REVIEW_STATUS_PUBLISHED, true},
		{nemgen.ProjectVersionReviewStatus_PROJECT_VERSION_REVIEW_STATUS_DRAFT, false},
		{nemgen.ProjectVersionReviewStatus_PROJECT_VERSION_REVIEW_STATUS_IN_REVIEW, false},
		{nemgen.ProjectVersionReviewStatus_PROJECT_VERSION_REVIEW_STATUS_REJECTED, false},
	} {
		got := planProjectVersion(&nemgen.ProjectVersion{Uuid: "v", Identifier: "v_8", ReviewStatus: tc.status})
		if got.Approved != tc.wantApproved {
			t.Errorf("status %v: Approved = %v, want %v", tc.status, got.Approved, tc.wantApproved)
		}
	}
	if got := planProjectVersion(nil); got != (planVersion{}) {
		t.Errorf("planProjectVersion(nil) = %+v", got)
	}
}

func TestDeployPlanReportJSONContract(t *testing.T) {
	// This JSON is the agent-facing contract of `deploy --plan --json`. Comparing
	// against a literal means an accidental field rename fails here rather than
	// silently breaking every consumer.
	plan := sqlplan.Analyze(`CREATE TABLE "clients" (uuid UUID PRIMARY KEY);
ALTER TABLE "public"."orders" DROP COLUMN "legacy_ref";`, sqlplan.EnginePostgres)

	report := deployPlanReport{
		Status:         "plan",
		Mode:           "diff",
		Project:        planProject{UUID: "p-1", Name: "acme"},
		ProjectVersion: planVersion{UUID: "v-1", Identifier: "v_8", ReviewStatus: "PUBLISHED", Approved: true},
		Target: planTargetReport{
			Source:                   "deployment acme-3f2a1c",
			DeploymentID:             "acme-3f2a1c",
			Mode:                     "local",
			Engine:                   "postgres",
			Schema:                   "public",
			LocalAgentUUID:           "agent-1",
			LocalAgentConnectionUUID: "conn-1",
		},
		Changes:       true,
		Destructive:   plan.HasDestructive(),
		Counts:        plan.Counts(),
		Statements:    plan.Statements,
		ApplySQL:      "CREATE TABLE \"clients\" (uuid UUID PRIMARY KEY);\nALTER TABLE \"public\".\"orders\" DROP COLUMN \"legacy_ref\";",
		Transactional: false,
		Caveats:       nil,
		Applied:       false,
		RerunCommand:  "nuzur-cli deploy --host prod --allow-destructive",
	}

	got, err := json.Marshal(report)
	if err != nil {
		t.Fatal(err)
	}
	want := `{"status":"plan","mode":"diff","project":{"uuid":"p-1","name":"acme"},` +
		`"project_version":{"uuid":"v-1","identifier":"v_8","review_status":"PUBLISHED","approved":true},` +
		`"target":{"source":"deployment acme-3f2a1c","deployment_id":"acme-3f2a1c","mode":"local",` +
		`"engine":"postgres","schema":"public","local_agent_uuid":"agent-1","local_agent_connection_uuid":"conn-1"},` +
		`"changes":true,"destructive":true,` +
		`"counts":{"total":2,"additive":1,"data_loss":1,"constraint_loss":0,"narrowing":0},` +
		`"statements":[` +
		`{"index":1,"sql":"CREATE TABLE \"clients\" (uuid UUID PRIMARY KEY)","kind":"create_table","object":"clients"},` +
		`{"index":2,"sql":"ALTER TABLE \"public\".\"orders\" DROP COLUMN \"legacy_ref\"","kind":"drop_column",` +
		`"severity":"data_loss","object":"public.orders.legacy_ref",` +
		`"reason":"drops legacy_ref from public.orders and every value in it",` +
		// Every hazard the statement carries, worst first. A single ALTER can lose
		// several different things at once, and the summary fields above are only the
		// worst of them; an agent reading this needs all of them.
		`"hazards":[{"severity":"data_loss","kind":"drop_column","object":"public.orders.legacy_ref",` +
		`"reason":"drops legacy_ref from public.orders and every value in it"}]}],` +
		`"apply_sql":"CREATE TABLE \"clients\" (uuid UUID PRIMARY KEY);\nALTER TABLE \"public\".\"orders\" DROP COLUMN \"legacy_ref\";",` +
		`"transactional":false,"applied":false,` +
		`"rerun_command":"nuzur-cli deploy --host prod --allow-destructive"}`

	if string(got) != want {
		t.Fatalf("plan JSON changed shape.\n got: %s\nwant: %s", got, want)
	}
}

func TestPlanTargetConfigValues(t *testing.T) {
	local := planTarget{Mode: connModeLocal, AgentUUID: "a", ConnUUID: "c", Schema: "public"}
	if got := local.configValues(); got["local_agent"] != "a" || got["local_agent_connection"] != "c" || got["local_agent_schema"] != "public" {
		t.Fatalf("local config = %+v", got)
	}
	if local.extensionIdentifier() != sqlPushPair.Local {
		t.Fatalf("local target picked %q", local.extensionIdentifier())
	}

	remote := planTarget{Mode: connModeRemote, TeamConnUUID: "tc", TeamStore: "st", Schema: "crm"}
	if got := remote.configValues(); got["connection"] != "tc" || got["store"] != "st" || got["schema"] != "crm" {
		t.Fatalf("remote config = %+v", got)
	}
	if remote.extensionIdentifier() != sqlPushPair.Front {
		t.Fatalf("remote target picked %q", remote.extensionIdentifier())
	}
}

func TestDeployPushTarget(t *testing.T) {
	// A --connection deploy is the only remote case; everything else goes through
	// the box's agent.
	remote := deployPushTarget("agent-1", "conn-1", "crm", "team-conn", "store", deploy.DBPostgres)
	if remote.Mode != connModeRemote || remote.TeamConnUUID != "team-conn" || remote.TeamStore != "store" {
		t.Fatalf("got %+v", remote)
	}
	local := deployPushTarget("agent-1", "conn-1", "acme", "", "", deploy.DBMySQL)
	if local.Mode != connModeLocal || local.AgentUUID != "agent-1" || local.ConnUUID != "conn-1" {
		t.Fatalf("got %+v", local)
	}
	if local.Engine != deploy.DBMySQL || local.Schema != "acme" {
		t.Fatalf("got %+v", local)
	}
}

// `--plan --deployment <id>` used to fail with "a project is required in
// non-interactive mode", contradicting the flag's own help — the record carries
// the project, so nothing about it has to be re-typed.
func TestPlanProjectRef(t *testing.T) {
	deps := []deploy.Deployment{planDep("dep-1", "h1", "app")}
	// A record written before ProjectUUID existed: nothing to derive from.
	legacy := planDep("dep-old", "h2", "app")
	legacy.ProjectUUID = ""

	for _, tc := range []struct {
		name         string
		flagProject  string
		deploymentID string
		deps         []deploy.Deployment
		wantRef      string
		wantDerived  string
		wantErrHas   string
	}{
		{
			name:         "the reported bug: --deployment alone supplies the project",
			deploymentID: "dep-1", deps: deps,
			wantRef: "project-1", wantDerived: "dep-1",
		},
		{
			// Explicit --project is the override, and it is not announced as derived.
			name: "an explicit --project wins", flagProject: "acme",
			deploymentID: "dep-1", deps: deps,
			wantRef: "acme",
		},
		{
			// Unchanged: without --deployment there is nothing to derive from, and the
			// caller's own "pass --project" error still applies.
			name: "no deployment leaves the flag alone", deps: deps, wantRef: "",
		},
		{
			// A typo'd id must not surface as the misleading "a project is required".
			name: "an unknown deployment names itself", deploymentID: "nope", deps: deps,
			wantErrHas: `no deployment "nope" on this machine`,
		},
		{
			name: "a record with no project says so", deploymentID: "dep-old",
			deps:       []deploy.Deployment{legacy},
			wantErrHas: "records no project",
		},
		{
			// The error is about the id, so it fires even with no records at all.
			name: "no records at all", deploymentID: "dep-1", deps: nil,
			wantErrHas: `no deployment "dep-1" on this machine`,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ref, derived, err := planProjectRef(tc.flagProject, tc.deploymentID, tc.deps)
			if tc.wantErrHas != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErrHas) {
					t.Fatalf("error = %v, want one containing %q", err, tc.wantErrHas)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if ref != tc.wantRef {
				t.Errorf("ref = %q, want %q", ref, tc.wantRef)
			}
			if derived != tc.wantDerived {
				t.Errorf("derivedFrom = %q, want %q", derived, tc.wantDerived)
			}
		})
	}
}

// A --project that contradicts the record is still refused — the override does not
// become a way to plan one project's schema against another's database. The check
// lives downstream, where both sides are resolved uuids (a --project may be a name).
func TestPlanProjectMismatchWithTheRecordIsStillRefused(t *testing.T) {
	dep := planDep("dep-1", "h1", "app")
	_, err := planTargetFromDeployment(&dep, "a-different-project", "")
	if err == nil || !strings.Contains(err.Error(), "belongs to project project-1") {
		t.Fatalf("expected a project-mismatch error, got %v", err)
	}
}

// ---------------------------------------------------------------------------
// The rerun command re-resolves the SAME target
// ---------------------------------------------------------------------------
//
// TestRerunCommand above pins what rerunCommand PRINTS. This pins what the
// printed command DOES, which is the guarantee the user actually relies on:
// paste the suggestion and the deploy writes to the database the plan just
// diffed — no other one. A suggestion that is well-formed but re-aims is worse
// than no suggestion, because the destructive plan hands it `--allow-destructive`.
//
// The two sides are DIFFERENT code paths, which is the whole reason to test it:
// `--plan` resolves its target with resolvePlanTargetFromState (a precedence
// ladder over the flags and the record store), while the deploy the suggestion
// runs resolves its target with applyDeploymentSelector + planIdentifier +
// deploySchemaName + pickPriorDeployment/pickBoxAgent + deployPushTarget. Nothing
// makes the two agree except that they read the same flags and the same records —
// so that is what is asserted, over one real record store.

const (
	rerunProjectUUID = "project-1"
	rerunProjectName = "Acme CRM"
)

// rerunDep is a complete, re-deployable record. All three of host, agent and
// connection are needed to reach a target at all: pickPriorDeployment skips a
// record with no agent, and planTargetFromDeployment refuses one with no
// connection. The agent is keyed on the HOST because that is what it is — one
// box, one agent, shared by every project on it (see pickBoxAgent).
func rerunDep(id, host, identifier string, engine deploy.DBEngine) deploy.Deployment {
	return deploy.Deployment{
		ID:             id,
		Provider:       deploy.ProviderSSH,
		Host:           host,
		User:           "root",
		Port:           22,
		Identifier:     identifier,
		ProjectUUID:    rerunProjectUUID,
		LocalAgentUUID: "agent-" + host,
		ConnUUID:       "conn-" + id,
		DBEngine:       engine,
		CreatedAt:      time.Date(2026, 7, 1, 12, 0, 0, 0, time.UTC),
	}
}

// rerunSeed isolates HOME and writes the records through the real store, then
// reads them back the way both invocations read them. Passing a literal slice
// would skip the round trip, and the round trip is part of the claim: the plan
// and the re-run agree because they see the same bytes on disk.
func rerunSeed(t *testing.T, deps ...deploy.Deployment) []deploy.Deployment {
	t.Helper()
	isolateHome(t)
	for idx := range deps {
		d := deps[idx]
		if err := deploy.SaveDeployment(&d); err != nil {
			t.Fatalf("seeding deployment %s: %v", d.ID, err)
		}
	}
	got, err := deploy.ListDeployments()
	if err != nil {
		t.Fatalf("listing the seeded deployments: %v", err)
	}
	if len(got) != len(deps) {
		t.Fatalf("seeded %d deployments, ListDeployments returned %d", len(deps), len(got))
	}
	return got
}

// rerunResolution is everything about "which database" that resolves without a
// network call.
type rerunResolution struct {
	projectRef string
	identifier string
	// target is the zero value when there is no live database yet (a first
	// deploy) — see rerunPlanSide.
	target planTarget
}

// rerunTargetKey is the part of a planTarget that decides which database gets
// written to.
//
// Source and DeploymentID are dropped because planTarget documents them as
// reporting-only and only the plan side fills them in. TeamStore is dropped for a
// different reason, and a weaker one: both sides read it from the same
// resolveConnectionForDeploy lookup, which is a network call this test does not
// make, so it is "" on both sides here rather than compared. See the --connection
// row for what that leaves uncovered.
func rerunTargetKey(pt planTarget) planTarget {
	pt.Source, pt.DeploymentID, pt.TeamStore = "", "", ""
	return pt
}

// rerunPlanSide runs runDeployPlan's targeting chain (command_deploy_plan.go:224-252)
// over `args`.
//
// Two links in that chain are network calls and are supplied as constants
// instead:
//
//   - the project (resolveRunTargets). Legitimate because WHICH project each
//     invocation resolves is decided by planProjectRef, which is pure and is
//     asserted equal across the two parses — given the same ref, the same server
//     returns the same uuid and name.
//   - the project's last-used generator config (lastGoCodeGenConfig), passed as
//     nil to BOTH sides for the reason planIdentifier's own doc comment gives:
//     the identifier comes from --identifier if it is set, and otherwise from
//     that same saved config on either side, so nil keeps them in step.
func rerunPlanSide(t *testing.T, args []string, deps []deploy.Deployment) rerunResolution {
	t.Helper()
	c := deployContext(t, args)
	s, err := resolveDeploySettings(c)
	if err != nil {
		t.Fatalf("plan side: resolving settings from %q: %v", args, err)
	}
	ref, _, err := planProjectRef(s.Project, c.String("deployment"), deps)
	if err != nil {
		t.Fatalf("plan side: planProjectRef: %v", err)
	}
	identifier := planIdentifier(s.Identifier, nil, rerunProjectName)
	target, err := resolvePlanTargetFromState(planTargetInput{
		DeploymentID:   c.String("deployment"),
		Host:           s.Host,
		Identifier:     identifier,
		TeamConnUUID:   s.Connection,
		DBDSN:          s.DBDSN,
		LocalAgent:     c.String("local-agent"),
		LocalAgentConn: c.String("local-agent-connection"),
		DBSchema:       s.DBSchema,
		DB:             s.DB,
		ProjectUUID:    rerunProjectUUID,
		Deployments:    deps,
	})
	if err != nil {
		t.Fatalf("plan side: resolving the target from %q: %v", args, err)
	}
	out := rerunResolution{projectRef: ref, identifier: identifier}
	if target != nil {
		out.target = *target
	}
	return out
}

// rerunDeploySide runs runDeploy's targeting sequence over `args` — the record
// selector (command_deploy.go:212-224), the engine (:279-297), the identifier
// (:469), the schema (:503), the prior deployment and the box agent (:564-577),
// and the push target itself (:1037).
//
// Two values in that sequence are not reachable purely, and both are inputs to
// deployPushTarget rather than derivations of the flags:
//
//   - the agent, when the box has never been paired. pickBoxAgent answers from
//     the records for every re-deploy, which is every case a plan can have a live
//     target for; a first deploy pairs a new one, and there is nothing to compare
//     it to (the plan has no target either — see the first-deploy row).
//   - connStore for --connection, from resolveConnectionForDeploy. Passed as ""
//     here, matching what the plan side has before its own call to the same
//     function.
func rerunDeploySide(t *testing.T, args []string, deps []deploy.Deployment) rerunResolution {
	t.Helper()
	c := deployContext(t, args)
	s, err := resolveDeploySettings(c)
	if err != nil {
		t.Fatalf("deploy side: resolving settings from %q: %v", args, err)
	}
	if depID := strings.TrimSpace(c.String("deployment")); depID != "" && !c.Bool("plan") {
		rec := findDeploymentByID(deps, depID)
		if rec == nil {
			t.Fatalf("deploy side: the suggestion names deployment %q, which is not in the store", depID)
		}
		if _, err := applyDeploymentSelector(s, rec, c.IsSet); err != nil {
			t.Fatalf("deploy side: applyDeploymentSelector: %v", err)
		}
	}
	// runDeploy reads --db only for a self-hosted database: --db-dsn takes its
	// engine from the DSN and --connection from the team connection.
	external := strings.TrimSpace(s.Connection) != "" || strings.TrimSpace(s.DBDSN) != ""
	engine := deploy.DBMySQL
	if !external && s.DB == string(deploy.DBPostgres) {
		engine = deploy.DBPostgres
	}
	identifier := planIdentifier(s.Identifier, nil, rerunProjectName)
	prior := pickPriorDeployment(deps, s.Host, identifier)
	// The --host engine adoption: with no engine stated anywhere and a record for
	// this host+identifier, the deploy takes the engine off that record rather
	// than off --db's mysql default. Without this the two sides read the engine
	// from different places and reached two different databases.
	if !s.DBStated && !external && strings.TrimSpace(s.Host) != "" &&
		prior != nil && prior.DBEngine != "" {
		engine = prior.DBEngine
	}
	connUUID := ""
	if prior != nil {
		connUUID = prior.ConnUUID
	}
	return rerunResolution{
		projectRef: s.Project,
		identifier: identifier,
		target: deployPushTarget(
			pickBoxAgent(deps, s.Host),
			connUUID,
			deploySchemaName(engine, sanitizeDBName(identifier), s.DBSchema),
			strings.TrimSpace(s.Connection),
			"",
			engine,
		),
	}
}

// rerunTokens splits a rendered rerun command back into argv, undoing shellQuote
// — the step the user performs by pasting it into a shell.
func rerunTokens(t *testing.T, cmd string) []string {
	t.Helper()
	var out []string
	var cur strings.Builder
	quoted, started := false, false
	for idx := 0; idx < len(cmd); idx++ {
		ch := cmd[idx]
		switch {
		case ch == '\'' && !quoted:
			quoted, started = true, true
		case ch == '\'' && quoted:
			// shellQuote escapes an embedded quote as '\'' — close, escaped
			// quote, reopen. Consume all four bytes and stay inside the quotes.
			if idx+3 < len(cmd) && cmd[idx+1] == '\\' && cmd[idx+2] == '\'' && cmd[idx+3] == '\'' {
				cur.WriteByte('\'')
				idx += 3
				continue
			}
			quoted = false
		case ch == ' ' && !quoted:
			if started {
				out = append(out, cur.String())
				cur.Reset()
				started = false
			}
		default:
			cur.WriteByte(ch)
			started = true
		}
	}
	if quoted {
		t.Fatalf("unbalanced quote in the suggestion %q", cmd)
	}
	if started {
		out = append(out, cur.String())
	}
	return out
}

func TestRerunCommandReResolvesSameTarget(t *testing.T) {
	pgBox := rerunDep("acme-3f2a1c", "prod-1", "acme", deploy.DBPostgres)
	myBox := rerunDep("shop-9b1c02", "prod-2", "shop", deploy.DBMySQL)

	for _, tc := range []struct {
		name string
		// argv is the full os.Args of the `--plan` invocation, program name
		// included — rerunCommand reads argv[0] and skips it.
		argv     []string
		deps     []deploy.Deployment
		addAllow bool
		// want pins the suggestion, so a row that stops re-resolving says which
		// of the two halves moved.
		want string
		// wantTarget is the database BOTH sides must land on, as a
		// rerunTargetKey (so the plan's reporting-only Source/DeploymentID are
		// not repeated here). Zero means there is no live database yet.
		wantTarget planTarget
		// wantIdentifier is asserted on both sides only when there is no live
		// target: it is then the whole of "which database", the name a first
		// deploy would create. With a live target the two sides legitimately
		// differ — see the --deployment row.
		wantIdentifier string
	}{
		{
			// The ordinary case: a re-deploy of a box this machine deployed
			// before, found by host + identifier on both sides.
			name: "a plain --host re-deploy",
			argv: []string{"nuzur-cli", "deploy", "--plan", "--host", "prod-2", "--identifier", "shop", "--project", "project-1"},
			deps: []deploy.Deployment{myBox},
			want: "nuzur-cli deploy --host prod-2 --identifier shop --project project-1",
			wantTarget: planTarget{
				Mode: connModeLocal, AgentUUID: "agent-prod-2", ConnUUID: "conn-shop-9b1c02",
				Schema: "shop", Engine: deploy.DBMySQL,
			},
		},
		{
			// Postgres moves the schema off the database name, so this row is
			// where a schema mismatch between the two sides would show up. It
			// also drops --json on the way through.
			name: "a --host re-deploy against Postgres",
			argv: []string{"nuzur-cli", "deploy", "--plan", "--json", "--db", "postgres", "--host", "prod-1", "--identifier", "acme", "--project", "project-1"},
			deps: []deploy.Deployment{pgBox},
			want: "nuzur-cli deploy --db postgres --host prod-1 --identifier acme --project project-1",
			wantTarget: planTarget{
				Mode: connModeLocal, AgentUUID: "agent-prod-1", ConnUUID: "conn-acme-3f2a1c",
				Schema: "public", Engine: deploy.DBPostgres,
			},
		},
		{
			// The selector rerunCommand exists to preserve. Nothing here is
			// typed: the record supplies the project, the host, the identifier
			// and the engine on the deploy side, and supplies the agent, the
			// connection and the engine directly on the plan side. Two entirely
			// different derivations of one database.
			name: "the --deployment selector, nothing else typed",
			argv: []string{"nuzur-cli", "deploy", "--plan", "--deployment", "acme-3f2a1c", "--version", "v_8"},
			deps: []deploy.Deployment{pgBox},
			want: "nuzur-cli deploy --deployment acme-3f2a1c --version v_8",
			wantTarget: planTarget{
				Mode: connModeLocal, AgentUUID: "agent-prod-1", ConnUUID: "conn-acme-3f2a1c",
				Schema: "public", Engine: deploy.DBPostgres,
			},
		},
		{
			// A team connection: nuzur reaches the database directly and no box
			// agent is involved, so the target has to come out remote on BOTH
			// sides. It does not, if --connection is ever dropped the way
			// --deployment once was — the deploy would then self-host a new
			// database on prod-2 instead.
			name: "a --connection deploy stays remote",
			argv: []string{"nuzur-cli", "deploy", "--plan", "--connection", "team-conn-9", "--host", "prod-2", "--identifier", "shop", "--project", "project-1"},
			deps: []deploy.Deployment{myBox},
			want: "nuzur-cli deploy --connection team-conn-9 --host prod-2 --identifier shop --project project-1",
			wantTarget: planTarget{
				Mode: connModeRemote, TeamConnUUID: "team-conn-9",
				Schema: "shop", Engine: deploy.DBMySQL,
			},
		},
		{
			// The reason any of this matters: a blocked destructive plan hands
			// the user a command carrying --allow-destructive. If it re-resolved
			// anywhere else, that flag would be pointed at a database nobody
			// diffed. (The append itself is checked once, below.)
			name:     "a gate-blocked plan, --allow-destructive appended",
			argv:     []string{"nuzur-cli", "deploy", "--plan", "--deployment", "acme-3f2a1c", "--version", "63fc3f92"},
			deps:     []deploy.Deployment{pgBox},
			addAllow: true,
			want:     "nuzur-cli deploy --deployment acme-3f2a1c --version 63fc3f92 --allow-destructive",
			wantTarget: planTarget{
				Mode: connModeLocal, AgentUUID: "agent-prod-1", ConnUUID: "conn-acme-3f2a1c",
				Schema: "public", Engine: deploy.DBPostgres,
			},
		},
		{
			// The engine dimension, and the row this table was missing. `--plan`
			// reads the engine off the RECORD; the deploy read it off --db, which
			// defaults to mysql — so this exact invocation used to diff schema
			// `public` on Postgres and then suggest a command that applied to
			// schema `acme` as MySQL. The deploy now adopts the recorded engine
			// when nothing states one, which is what makes both sides land on the
			// same database without the user repeating --db.
			name:     "a --host re-deploy against Postgres without repeating --db",
			argv:     []string{"nuzur-cli", "deploy", "--plan", "--host", "prod-1", "--identifier", "acme", "--project", "project-1"},
			deps:     []deploy.Deployment{pgBox},
			addAllow: true,
			want:     "nuzur-cli deploy --host prod-1 --identifier acme --project project-1 --allow-destructive",
			wantTarget: planTarget{
				Mode: connModeLocal, AgentUUID: "agent-prod-1", ConnUUID: "conn-acme-3f2a1c",
				Schema: "public", Engine: deploy.DBPostgres,
			},
		},
		{
			// No record for this host+identifier: the plan has nothing to diff
			// and reports the CREATE script instead. There is no target to
			// compare, but the name of the database that script would create
			// still has to survive into the suggestion.
			name:           "a first deploy has no live target, and the re-run names the same database",
			argv:           []string{"nuzur-cli", "deploy", "--plan", "--host", "prod-9", "--identifier", "fresh", "--project", "project-1"},
			deps:           []deploy.Deployment{myBox},
			want:           "nuzur-cli deploy --host prod-9 --identifier fresh --project project-1",
			wantIdentifier: "fresh",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			deps := rerunSeed(t, tc.deps...)
			planned := rerunPlanSide(t, tc.argv[2:], deps)

			got := rerunCommand(tc.argv, tc.addAllow)
			if got != tc.want {
				t.Fatalf("rerunCommand() = %q, want %q", got, tc.want)
			}
			tokens := rerunTokens(t, got)
			if len(tokens) < 2 || tokens[0] != "nuzur-cli" || tokens[1] != "deploy" {
				t.Fatalf("suggestion %q tokenizes to %q, want it to start with `nuzur-cli deploy`", got, tokens)
			}
			applied := rerunDeploySide(t, tokens[2:], deps)

			if planned.projectRef != applied.projectRef {
				t.Errorf("project: plan resolved %q, the suggested re-run resolves %q", planned.projectRef, applied.projectRef)
			}
			if key := rerunTargetKey(planned.target); key != tc.wantTarget {
				t.Errorf("plan target = %+v, want %+v", key, tc.wantTarget)
			}
			if (tc.wantTarget == planTarget{}) {
				// No live database. The identifier IS the target.
				if planned.identifier != tc.wantIdentifier || applied.identifier != tc.wantIdentifier {
					t.Errorf("identifier: plan %q, re-run %q, want %q on both",
						planned.identifier, applied.identifier, tc.wantIdentifier)
				}
				return
			}
			if key, want := rerunTargetKey(applied.target), rerunTargetKey(planned.target); key != want {
				t.Errorf("the suggested re-run applies to a DIFFERENT database.\n  planned: %+v\n  re-run:  %+v", want, key)
			}
		})
	}
}

// The append is one line of rerunCommand and is checked once, here, rather than
// per row: what has to hold is that it adds the permission and changes nothing
// about where that permission is spent.
func TestRerunCommandAllowDestructiveOnlyAddsThePermission(t *testing.T) {
	deps := rerunSeed(t, rerunDep("acme-3f2a1c", "prod-1", "acme", deploy.DBPostgres))
	argv := []string{"nuzur-cli", "deploy", "--plan", "--deployment", "acme-3f2a1c"}

	plain, allowed := rerunCommand(argv, false), rerunCommand(argv, true)
	if allowed != plain+" --allow-destructive" {
		t.Fatalf("with the gate blocked the suggestion is %q, want %q plus a trailing --allow-destructive", allowed, plain)
	}
	plainTokens, allowedTokens := rerunTokens(t, plain), rerunTokens(t, allowed)

	// The permission is actually granted: the flag parses as set, which is what
	// s.AllowDestructive reads.
	if c := deployContext(t, plainTokens[2:]); c.Bool("allow-destructive") {
		t.Error("the plain suggestion already grants --allow-destructive")
	}
	if c := deployContext(t, allowedTokens[2:]); !c.Bool("allow-destructive") {
		t.Error("the appended --allow-destructive does not parse as set")
	}
	// And it is spent on the same database.
	before := rerunTargetKey(rerunDeploySide(t, plainTokens[2:], deps).target)
	after := rerunTargetKey(rerunDeploySide(t, allowedTokens[2:], deps).target)
	if before != after {
		t.Errorf("--allow-destructive moved the target.\n  without: %+v\n  with:    %+v", before, after)
	}
}

// Both gaps this file used to pin are now fixed, in two different places, and
// what replaces them are tests of the fixes. The one below is GAP 1's; GAP 2's is
// the "--host re-deploy against Postgres without repeating --db" row in the table
// above, where it belongs — that fix made the two sides agree, so the case became
// an ordinary row.
//
// GAP 1 was: rerunCommand drops --local-agent/--local-agent-connection with their
// values, and those flags are precedence 1 in resolvePlanTargetFromState — they
// ARE the planned target. Once dropped, the re-run fell through to the
// host+identifier lookup and pushed to whatever connection the record for that box
// happened to hold, carrying --allow-destructive when the plan was destructive.
//
// The fix is not to keep the flags: a deploy really cannot use them, it reaches
// its database through the box it deploys to. It is to stop offering a command at
// all on that path, and say why — see localAgentRerunNote. The rule being kept is
// the one this whole section exists for: a suggestion applies to what was planned,
// or there is no suggestion.
func TestLocalAgentTargetIsOfferedNoRerunCommand(t *testing.T) {
	deps := rerunSeed(t, rerunDep("shop-9b1c02", "prod-2", "shop", deploy.DBMySQL))
	argv := []string{"nuzur-cli", "deploy", "--plan",
		"--local-agent", "some-other-agent", "--local-agent-connection", "some-other-conn",
		"--host", "prod-2", "--identifier", "shop", "--project", "project-1"}

	// The plan really does target the flagged connection — precedence 1 beating
	// the host+identifier record that is also present.
	planned := rerunPlanSide(t, argv[2:], deps)
	if planned.target.ConnUUID != "some-other-conn" {
		t.Fatalf("the plan did not target the flagged connection: %+v", planned.target)
	}

	// So no command is offered.
	c := deployContext(t, argv[2:])
	in := planTargetInput{
		Host:           "prod-2",
		Identifier:     "shop",
		LocalAgent:     c.String("local-agent"),
		LocalAgentConn: c.String("local-agent-connection"),
		Deployments:    deps,
	}
	if !targetChosenByLocalAgentFlags(in) {
		t.Fatal("the --local-agent pair was not recognised as the source of the target")
	}

	// And the reason still holds: had one been offered, it would have re-aimed.
	// This is the assertion that keeps the suppression honest — if the two ever
	// come to agree, the suppression can go, and this line says so.
	applied := rerunDeploySide(t, rerunTokens(t, rerunCommand(argv, true))[2:], deps)
	if applied.target.ConnUUID != "conn-shop-9b1c02" {
		t.Fatalf("re-run target = %+v", applied.target)
	}
	if applied.target.ConnUUID == planned.target.ConnUUID {
		t.Fatal("a re-run now resolves the same connection the --local-agent pair named — " +
			"the suppression is no longer needed and this case belongs in the table above")
	}

	// The note has to carry the two things the absent command carried: which
	// database was planned, and how to get one that can be pasted.
	note := localAgentRerunNote("some-other-conn")
	for _, must := range []string{"some-other-conn", "--local-agent", "--deployment"} {
		if !strings.Contains(note, must) {
			t.Errorf("the note does not mention %q:\n%s", must, note)
		}
	}
}

// The two fields are alternatives, and a consumer — including an agent reading
// the JSON — relies on that: no rerun_command means there is a rerun_note saying
// why, never silence and never both.
func TestPlanReportOffersACommandOrANoteNeverBoth(t *testing.T) {
	var out, errOut bytes.Buffer
	swapOutputWriters(t, &out, &errOut)

	plan := sqlplan.Analyze("ALTER TABLE `customer` DROP COLUMN `legacy_ref`;", sqlplan.EngineMySQL)
	report := deployPlanReport{
		Status: "plan", Mode: "diff",
		Project:        planProject{UUID: "p-1", Name: "acme"},
		ProjectVersion: planVersion{UUID: "v-1", Identifier: "v_8", ReviewStatus: "PUBLISHED", Approved: true},
		Target:         planTargetReport{Source: "--local-agent/--local-agent-connection flags", Engine: "mysql", Schema: "shop"},
		Changes:        true,
		Destructive:    true,
		Statements:     plan.Statements,
		RerunNote:      localAgentRerunNote("some-other-conn"),
	}
	printDeployPlan(report, plan)

	printed := errOut.String()
	if strings.Contains(printed, "To apply it:") {
		t.Errorf("a plan with no rerun command still printed one:\n%s", printed)
	}
	if !strings.Contains(printed, "No re-run command is offered") {
		t.Errorf("the note was not printed:\n%s", printed)
	}
	// The destructive warning still stands: the permission is still required, it
	// simply cannot be handed over pre-typed.
	if !strings.Contains(printed, "--allow-destructive is required") {
		t.Errorf("the destructive warning was dropped along with the command:\n%s", printed)
	}
}

// KNOWN GAP — asserts the CURRENT, WRONG behavior so the day it is fixed this
// test fails and this comment is read. It is not a reason to weaken anything
// above.
//
// The engine adoption fixed the case where the user says NOTHING about the
// engine. It deliberately did not touch the case where they say something that
// contradicts the record, because "an explicit flag always wins" is the rule the
// whole selector is built on (see applyDeploymentSelector) and reversing it here
// would be a bigger decision than this one.
//
// The result is a residual disagreement: `--plan` reads the engine off the record
// whatever --db says (planTargetFromDeployment does not look at flags), while the
// deploy honours --db. So `--plan --db mysql` against a Postgres box diffs schema
// `public` as Postgres and then suggests a command that deploys schema `acme` as
// MySQL — onto a box that already runs Postgres.
//
// The fix, when it is taken, probably belongs on the deploy side and probably is a
// REFUSAL rather than an adoption: re-deploying a recorded Postgres box as MySQL
// installs a second engine and applies the schema to a database that does not
// exist, which is not something a flag should be able to ask for by accident.
func TestRerunCommandReResolvesSameTarget_knownGaps(t *testing.T) {
	t.Run("an explicit --db contradicting the record splits the two sides", func(t *testing.T) {
		deps := rerunSeed(t, rerunDep("acme-3f2a1c", "prod-1", "acme", deploy.DBPostgres))
		argv := []string{"nuzur-cli", "deploy", "--plan", "--db", "mysql",
			"--host", "prod-1", "--identifier", "acme", "--project", "project-1"}

		planned := rerunPlanSide(t, argv[2:], deps)
		applied := rerunDeploySide(t, rerunTokens(t, rerunCommand(argv, true))[2:], deps)

		if planned.target.Engine != deploy.DBPostgres || planned.target.Schema != "public" {
			t.Fatalf("plan target = %+v, want the record's Postgres engine and schema public", planned.target)
		}
		if applied.target.Engine != deploy.DBMySQL || applied.target.Schema != "acme" {
			t.Fatalf("re-run target = %+v, want the stated mysql and schema acme", applied.target)
		}
		if rerunTargetKey(planned.target) == rerunTargetKey(applied.target) {
			t.Fatal("the gap looks fixed — delete this test and add the case to the table above")
		}
	})
}
