package app

import (
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
			// The value has to go with the flag, or it lands as a stray argument.
			name: "a plan selector drops its separated value too",
			argv: []string{"nuzur-cli", "deploy", "--plan", "--deployment", "acme-3f2a1c", "--host", "prod"},
			want: "nuzur-cli deploy --host prod",
		},
		{
			name: "a plan selector drops its inline value too",
			argv: []string{"nuzur-cli", "deploy", "--plan", "--deployment=acme-3f2a1c", "--host", "prod"},
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
ALTER TABLE "public"."orders" DROP COLUMN "legacy_ref";`)

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
		`"reason":"drops legacy_ref from public.orders and every value in it"}],` +
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
