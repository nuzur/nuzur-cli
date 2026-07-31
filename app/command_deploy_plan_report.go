package app

import (
	"fmt"
	"os"
	"slices"
	"strings"

	"github.com/nuzur/nuzur-cli/outputtools"
	"github.com/nuzur/nuzur-cli/sqlplan"
)

// caveatMySQLChurn marks a plan whose "existing" side was reconstructed rather than
// read, and which can therefore contain statements that change nothing.
const caveatMySQLChurn = "mysql_phantom_churn"

// deployPlanReport is the machine-readable plan, and the agent-facing contract of
// `deploy --plan --json`.
//
// ApplySQL verbatim is the load-bearing field: an agent that wants to reason about
// the migration should read that, not re-derive it from Statements. Destructive and
// Caveats are the decision fields. Applied is always false — a plan applies nothing,
// and saying so explicitly means a consumer never has to infer it.
type deployPlanReport struct {
	Status string `json:"status"` // always "plan"
	// Mode is "diff" against a live database, or "create" when there is no
	// database yet and this is the script a first deploy would run.
	Mode           string              `json:"mode"`
	Project        planProject         `json:"project"`
	ProjectVersion planVersion         `json:"project_version"`
	Target         planTargetReport    `json:"target"`
	Changes        bool                `json:"changes"`
	Message        string              `json:"message,omitempty"`
	Destructive    bool                `json:"destructive"`
	Counts         sqlplan.Counts      `json:"counts"`
	Statements     []sqlplan.Statement `json:"statements"`
	ApplySQL       string              `json:"apply_sql"`
	// Transactional records whether the whole migration is applied as one unit. True
	// only on Postgres, and only when nothing in the plan forces the executor off the
	// transactional path (CONCURRENTLY index operations and friends). False means a
	// failure partway through can leave the database partly migrated — on MySQL that
	// is always the case, because DDL commits implicitly.
	Transactional bool     `json:"transactional"`
	Caveats       []string `json:"caveats,omitempty"`
	Applied       bool     `json:"applied"`
	RerunCommand  string   `json:"rerun_command,omitempty"`
}

type planProject struct {
	UUID string `json:"uuid"`
	Name string `json:"name"`
}

type planVersion struct {
	UUID         string `json:"uuid"`
	Identifier   string `json:"identifier"`
	ReviewStatus string `json:"review_status"`
	Approved     bool   `json:"approved"`
}

type planTargetReport struct {
	// Source says how this target was chosen, so a reader can tell a plan against a
	// recorded deployment from one against flags they just typed.
	Source                   string `json:"source"`
	DeploymentID             string `json:"deployment_id,omitempty"`
	Mode                     string `json:"mode,omitempty"` // "local" (via agent) | "remote"
	Engine                   string `json:"engine,omitempty"`
	Schema                   string `json:"schema,omitempty"`
	LocalAgentUUID           string `json:"local_agent_uuid,omitempty"`
	LocalAgentConnectionUUID string `json:"local_agent_connection_uuid,omitempty"`
	TeamConnectionUUID       string `json:"team_connection_uuid,omitempty"`
	TeamConnectionStoreUUID  string `json:"team_connection_store_uuid,omitempty"`
}

// planTargetReportFrom renders a resolved target for the report.
func planTargetReportFrom(t planTarget) planTargetReport {
	return planTargetReport{
		Source:                   t.Source,
		DeploymentID:             t.DeploymentID,
		Mode:                     string(t.Mode),
		Engine:                   string(t.Engine),
		Schema:                   t.Schema,
		LocalAgentUUID:           t.AgentUUID,
		LocalAgentConnectionUUID: t.ConnUUID,
		TeamConnectionUUID:       t.TeamConnUUID,
		TeamConnectionStoreUUID:  t.TeamStore,
	}
}

// printDeployPlan writes the human form of a plan.
//
// Narration goes to stderr and the plan body to stdout, following the convention
// the rest of the CLI uses, so `deploy --plan > migration.sql` yields something
// you can read and `2>/dev/null` yields something close to something you could run.
func printDeployPlan(r deployPlanReport, plan sqlplan.Plan) {
	version := r.ProjectVersion.Identifier
	if version == "" {
		version = r.ProjectVersion.UUID
	}
	outputtools.PrintlnColoredErr(fmt.Sprintf("\nPlan for project %q, version %s (%s)",
		r.Project.Name, version, r.ProjectVersion.ReviewStatus), outputtools.Blue)
	outputtools.PrintlnColoredErr("Target: "+planTargetLine(r.Target), outputtools.Blue)

	// A plan built from an unapproved version is useful but is not what a deploy
	// would do, because deploy refuses unapproved versions outright.
	if !r.ProjectVersion.Approved {
		outputtools.PrintlnColoredErr(fmt.Sprintf(
			"\nThis version is %s. The plan below is real, but `deploy` will refuse it until the\n"+
				"version is approved or published — approve it first, then deploy.", r.ProjectVersion.ReviewStatus),
			outputtools.Yellow)
	}

	if r.Mode == "create" {
		outputtools.PrintlnColoredErr("\nThere is no live database for this target yet — what follows is the CREATE script\n"+
			"a first deploy would run, not a diff.", outputtools.Yellow)
	}

	if !r.Changes {
		msg := strings.TrimSpace(r.Message)
		if msg == "" {
			msg = plan.SummaryLine()
		}
		outputtools.PrintlnColoredErr("\n"+msg, outputtools.Green)
		outputtools.PrintlnColoredErr("Nothing to apply — the database already matches this version of the model.", outputtools.Green)
		return
	}

	fmt.Fprintln(os.Stderr)
	outputtools.PrintlnColoredErr(plan.SummaryLine(), outputtools.Blue)

	if r.Destructive {
		fmt.Fprintln(os.Stderr)
		outputtools.PrintlnColoredErr(plan.RenderDestructive(), outputtools.Red)
	}

	if churn := plan.ChurnNote(); churn != "" && hasCaveat(r.Caveats, caveatMySQLChurn) {
		fmt.Fprintln(os.Stderr)
		outputtools.PrintlnColoredErr(churn, outputtools.Yellow)
	}
	if hasCaveat(r.Caveats, caveatMySQLChurn) {
		fmt.Fprintln(os.Stderr)
		outputtools.PrintlnColoredErr(sqlplan.MySQLCaveat(), outputtools.Yellow)
	}

	if w := plan.TransactionalWarning(sqlplan.Engine(r.Target.Engine)); w != "" {
		fmt.Fprintln(os.Stderr)
		outputtools.PrintlnColoredErr(w, outputtools.Yellow)
	}

	// The blast-radius bound: worth saying because nobody currently knows it, and
	// because a user staring at a DROP is exactly who needs to hear what CANNOT be
	// dropped.
	fmt.Fprintln(os.Stderr)
	outputtools.PrintlnColoredErr(sqlplan.DropOnlyWhatItCouldCreate(), outputtools.Gray)

	// The body, on stdout.
	fmt.Fprintln(os.Stderr, "\nFull plan, in execution order:")
	fmt.Println(plan.RenderStatements())

	fmt.Fprintln(os.Stderr)
	outputtools.PrintlnColoredErr("Nothing was applied — this was a dry run.", outputtools.Green)
	if r.RerunCommand != "" {
		outputtools.PrintlnColoredErr("To apply it:  "+r.RerunCommand, outputtools.Green)
	}
	if r.Destructive {
		outputtools.PrintlnColoredErr(
			"--allow-destructive is required because this plan deletes data. Without it the deploy\n"+
				"applies nothing.", outputtools.Yellow)
	}
}

// planTargetLine renders a target for the plan header.
func planTargetLine(t planTargetReport) string {
	parts := []string{t.Source}
	if t.Engine != "" {
		parts = append(parts, t.Engine)
	}
	if t.Schema != "" {
		parts = append(parts, fmt.Sprintf("schema %q", t.Schema))
	}
	switch {
	case t.LocalAgentUUID != "":
		parts = append(parts, "via agent "+t.LocalAgentUUID, "connection "+t.LocalAgentConnectionUUID)
	case t.TeamConnectionUUID != "":
		parts = append(parts, "team connection "+t.TeamConnectionUUID)
	}
	return strings.Join(parts, " — ")
}

func hasCaveat(caveats []string, want string) bool {
	return slices.Contains(caveats, want)
}
