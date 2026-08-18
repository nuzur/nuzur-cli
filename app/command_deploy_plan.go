package app

import (
	"errors"
	"fmt"
	"os"
	"strings"

	nemgen "github.com/nuzur/nem/idl/gen"
	"github.com/nuzur/nuzur-cli/deploy"
	"github.com/nuzur/nuzur-cli/extensionrun"
	"github.com/nuzur/nuzur-cli/outputtools"
	"github.com/nuzur/nuzur-cli/sqlplan"
	"github.com/urfave/cli"
)

// deployPlanResolveOptions is deployResolveOptions minus the approval gate.
//
// A plan writes nothing, so the reason deploy requires an approved version — what
// runs in production must be what a reviewer signed off on — does not apply to it.
// The case this exists for is the inverse: a database has drifted, the schema that
// would reconcile it is still a draft, and you need to see what that draft would do
// BEFORE spending a review cycle on it. A draft is visible only to its creator, so
// this widens no access, and the plan header names the review status on every run,
// so a draft plan cannot be mistaken for what a deploy would do. Deploy itself
// still refuses anything unapproved.
//
// checkLimit is deliberately not set: the field is no longer read anywhere.
func deployPlanResolveOptions() resolveOptions {
	return resolveOptions{
		extensionIdentifier:    goCodeGenExtensionIdentifier,
		interactive:            false,
		checkAccess:            true,
		requireApprovedVersion: false,
	}
}

// planTargetInput is everything the target selection needs that costs no network
// call. Deployment records are passed in rather than read so the policy below stays
// pure and testable.
type planTargetInput struct {
	DeploymentID   string // --deployment
	Host           string // --host
	Identifier     string // derived, see planIdentifier
	TeamConnUUID   string // --connection
	DBDSN          string // --db-dsn
	LocalAgent     string // --local-agent
	LocalAgentConn string // --local-agent-connection
	DBSchema       string // --db-schema
	DB             string // --db
	ProjectUUID    string
	Deployments    []deploy.Deployment
}

// resolvePlanTargetFromState picks the live database a plan should be computed
// against.
//
// Returns (nil, nil) when there is no live target — a first deploy — which the
// caller reports differently rather than treating as an error.
//
// Precedence, most explicit first:
//  1. --local-agent + --local-agent-connection
//  2. --deployment <id>
//  3. --connection <uuid> (remote: nuzur reaches the database directly)
//  4. host + identifier, the same lookup a real deploy does
//  5. nothing
func resolvePlanTargetFromState(in planTargetInput) (*planTarget, error) {
	engine := deploy.DBEngine(strings.TrimSpace(in.DB))
	if engine == "" {
		engine = deploy.DBMySQL
	}

	// 1. Explicit agent + connection. Both or neither: with only one there is no
	// way to guess the other, and guessing would aim the plan at some other
	// database.
	agent, conn := strings.TrimSpace(in.LocalAgent), strings.TrimSpace(in.LocalAgentConn)
	switch {
	case agent != "" && conn == "":
		return nil, errors.New("--local-agent needs --local-agent-connection (the agent can serve several databases; the connection says which one)")
	case conn != "" && agent == "":
		return nil, errors.New("--local-agent-connection needs --local-agent (the connection is registered on a specific agent)")
	case agent != "" && conn != "":
		return &planTarget{
			Mode:      connModeLocal,
			AgentUUID: agent,
			ConnUUID:  conn,
			Schema:    deploySchemaName(engine, sanitizeDBName(in.Identifier), in.DBSchema),
			Engine:    engine,
			Source:    "--local-agent/--local-agent-connection flags",
		}, nil
	}

	// 2. A recorded deployment, by id. This is the reliable path: the record holds
	// the agent, the connection and the engine, so nothing is re-derived.
	if id := strings.TrimSpace(in.DeploymentID); id != "" {
		dep := findDeploymentByID(in.Deployments, id)
		if dep == nil {
			return nil, fmt.Errorf("no deployment %q on this machine (see `nuzur-cli deploy list`)", id)
		}
		return planTargetFromDeployment(dep, in.ProjectUUID, in.DBSchema)
	}

	// 3. An existing team connection: nuzur reaches it directly, no agent involved.
	if tc := strings.TrimSpace(in.TeamConnUUID); tc != "" {
		return &planTarget{
			Mode:         connModeRemote,
			TeamConnUUID: tc,
			Schema:       deploySchemaName(engine, sanitizeDBName(in.Identifier), in.DBSchema),
			Engine:       engine,
			Source:       "--connection " + tc,
		}, nil
	}

	// 4. host + identifier — the lookup a real deploy performs to detect a
	// re-deploy. If it finds a box, that box is what this deploy would push to.
	if host := strings.TrimSpace(in.Host); host != "" {
		if dep := pickPriorDeployment(in.Deployments, host, in.Identifier); dep != nil {
			return planTargetFromDeployment(dep, in.ProjectUUID, in.DBSchema)
		}
	}

	// 5. Nothing to diff against. A raw DSN is the one case worth explaining,
	// because the user did name a database — it is simply not reachable from here.
	if strings.TrimSpace(in.DBDSN) != "" {
		return nil, errors.New("--plan cannot reach a database given only as --db-dsn: the diff runs either through a box's agent or against a nuzur team connection, and a raw DSN is neither. Plan it with --deployment <id> if this machine deployed it before, or with --local-agent/--local-agent-connection, or register it as a team connection and use --connection")
	}
	return nil, nil
}

// planTargetFromDeployment turns a recorded deployment into a plan target.
func planTargetFromDeployment(dep *deploy.Deployment, projectUUID, dbSchemaFlag string) (*planTarget, error) {
	// The same guard a real deploy applies: one identifier on one host belongs to
	// one project, or they would share a derived database name.
	if projectUUID != "" && dep.ProjectUUID != "" && dep.ProjectUUID != projectUUID {
		return nil, fmt.Errorf("deployment %q belongs to project %s, not the project being planned (%s) — plan the project that owns it, or pass a different --deployment", dep.ID, dep.ProjectUUID, projectUUID)
	}
	if dep.LocalAgentUUID == "" {
		return nil, fmt.Errorf("deployment %q has no paired agent recorded, so there is nothing to reach its database through (the deploy that created it did not finish pairing) — pass --local-agent/--local-agent-connection, or --connection", dep.ID)
	}
	if dep.ConnUUID == "" {
		return nil, fmt.Errorf("deployment %q has no database connection recorded — pass --local-agent-connection with the connection uuid", dep.ID)
	}
	engine := dep.DBEngine
	if engine == "" {
		engine = deploy.DBMySQL
	}
	// The recorded identifier is what named the database on the box.
	return &planTarget{
		Mode:         connModeLocal,
		AgentUUID:    dep.LocalAgentUUID,
		ConnUUID:     dep.ConnUUID,
		Schema:       deploySchemaName(engine, sanitizeDBName(dep.Identifier), dbSchemaFlag),
		Engine:       engine,
		DeploymentID: dep.ID,
		Source:       "deployment " + dep.ID,
	}, nil
}

// findDeploymentByID looks a deployment up in a list by exact id.
func findDeploymentByID(deps []deploy.Deployment, id string) *deploy.Deployment {
	for idx := range deps {
		if deps[idx].ID == id {
			return &deps[idx]
		}
	}
	return nil
}

// planProjectRef decides which project `--plan` resolves, from --project and
// --deployment.
//
// `--plan --deployment <id>` used to fail with "a project is required in
// non-interactive mode", which contradicted the flag's own help — "the record
// carries the agent, the connection and the engine, so nothing has to be
// re-derived from flags" — and the record carries the project too. Anything the
// record already knows should not have to be re-typed alongside it.
//
// Returns (ref, derivedFrom, err). derivedFrom is the deployment id when the
// project came from the record, so the caller can say where it got it; deriving a
// project silently would be its own small version of the same problem.
//
// An explicit --project still wins: it is the override, and a contradiction
// between it and the record is caught downstream by planTargetFromDeployment,
// which can compare resolved UUIDs (a --project may be a NAME, which is not
// comparable to the uuid on the record until it has been resolved).
func planProjectRef(flagProject, deploymentID string, deps []deploy.Deployment) (ref, derivedFrom string, err error) {
	flagProject = strings.TrimSpace(flagProject)
	deploymentID = strings.TrimSpace(deploymentID)
	if flagProject != "" || deploymentID == "" {
		return flagProject, "", nil
	}
	dep := findDeploymentByID(deps, deploymentID)
	if dep == nil {
		// The same message resolvePlanTargetFromState would give, said here because
		// this is where the id is first used — otherwise a typo'd id would surface as
		// the misleading "a project is required".
		return "", "", fmt.Errorf("no deployment %q on this machine (see `nuzur-cli deploy list`)", deploymentID)
	}
	if strings.TrimSpace(dep.ProjectUUID) == "" {
		return "", "", fmt.Errorf("deployment %q records no project (it predates the field), so the project cannot be derived from it — pass --project <name|uuid>", deploymentID)
	}
	return dep.ProjectUUID, dep.ID, nil
}

// runDeployPlan is `deploy --plan`: work out what the deploy would apply, print
// it, and exit having changed nothing.
func (i *Implementation) runDeployPlan(c *cli.Context, s *deploySettings) error {
	jsonOut := c.Bool("json")

	// Records are read BEFORE the project is resolved, because --deployment can
	// supply the project. See planProjectRef.
	deps, err := deploy.ListDeployments()
	if err != nil {
		// Not fatal: the explicit selectors do not need local state at all.
		deps = nil
	}

	projectRef, derivedFrom, err := planProjectRef(s.Project, c.String("deployment"), deps)
	if err != nil {
		return err
	}
	if derivedFrom != "" {
		outputtools.PrintlnColoredErr(fmt.Sprintf(
			"Planning project %s, taken from deployment %s (no --project given).", projectRef, derivedFrom), outputtools.Blue)
	}

	targets, err := i.resolveRunTargets(extRunFlags{
		project:        projectRef,
		version:        s.Version,
		nonInteractive: true,
	}, deployPlanResolveOptions())
	if err != nil {
		return err
	}

	identifier := planIdentifier(s.Identifier, lastGoCodeGenConfig(targets), targets.project.Name)

	targetInput := planTargetInput{
		DeploymentID:   c.String("deployment"),
		Host:           s.Host,
		Identifier:     identifier,
		TeamConnUUID:   s.Connection,
		DBDSN:          s.DBDSN,
		LocalAgent:     c.String("local-agent"),
		LocalAgentConn: c.String("local-agent-connection"),
		DBSchema:       s.DBSchema,
		DB:             s.DB,
		ProjectUUID:    targets.project.Uuid,
		Deployments:    deps,
	}
	target, err := resolvePlanTargetFromState(targetInput)
	if err != nil {
		return err
	}

	// A --connection target needs its store and engine resolved before sql-push can
	// reach it — the same lookup a real deploy performs for --connection.
	if target != nil && target.Mode == connModeRemote && target.TeamStore == "" {
		engine, _, _, _, _, _, _, store, err := i.resolveConnectionForDeploy(target.TeamConnUUID, targets.project.TeamUuid)
		if err != nil {
			return err
		}
		target.TeamStore = store
		if engine != "" {
			target.Engine = engine
			// A Postgres connection's schema defaults differently from MySQL's, and
			// the engine was unknown when the target was first assembled.
			target.Schema = deploySchemaName(engine, target.Schema, s.DBSchema)
		}
	}

	// Whether a pasteable command can be offered at all, decided once — see
	// targetChosenByLocalAgentFlags.
	suggestRerun := !targetChosenByLocalAgentFlags(targetInput)

	report := deployPlanReport{
		Status:         "plan",
		Project:        planProject{UUID: targets.project.Uuid, Name: targets.project.Name},
		ProjectVersion: planProjectVersion(targets.projectVersion),
		Applied:        false,
	}
	if suggestRerun {
		report.RerunCommand = rerunCommand(os.Args, false)
	} else {
		report.RerunNote = localAgentRerunNote(targetInput.LocalAgentConn)
	}

	var plan sqlplan.Plan
	switch {
	case target == nil:
		// No live database yet. The honest answer is not "nothing to say" — it is
		// the CREATE script a first deploy would run.
		report.Mode = "create"
		engine := deploy.DBEngine(strings.TrimSpace(s.DB))
		if engine == "" {
			engine = deploy.DBMySQL
		}
		report.Target = planTargetReport{Source: "no live database yet", Engine: string(engine)}
		createSQL, err := i.computeCreatePlan(targets, engine)
		if err != nil {
			return err
		}
		plan = sqlplan.Analyze(createSQL, sqlplan.Engine(engine))
		report.ApplySQL = createSQL

	default:
		report.Mode = "diff"
		report.Target = planTargetReportFrom(*target)
		applySQL, message, err := i.computeSchemaPlan(targets, *target)
		if err != nil {
			return err
		}
		plan = sqlplan.Analyze(applySQL, sqlplan.Engine(report.Target.Engine))
		report.ApplySQL = applySQL
		report.Message = message
	}

	report.Changes = !plan.Empty()
	report.Destructive = plan.HasDestructive()
	report.Counts = plan.Counts()
	report.Statements = plan.Statements
	// Reported, not assumed: sql-push asks for a transaction, which Postgres honors
	// unless the batch contains something it cannot run inside one, and which MySQL
	// cannot honor for DDL at all.
	report.Transactional = plan.Transactional(sqlplan.Engine(report.Target.Engine))
	if report.Destructive && suggestRerun {
		report.RerunCommand = rerunCommand(os.Args, true)
	}
	if isMySQL(report.Target.Engine) {
		report.Caveats = append(report.Caveats, caveatMySQLChurn)
	}

	if jsonOut {
		return printJSONValue(report)
	}
	printDeployPlan(report, plan)
	return nil
}

// computeSchemaPlan asks sql-push for the diff and then REJECTS its confirmation
// step, so the extension ends having executed nothing.
//
// The rejection IS the dry run. sql-push has no dry-run mode, but its confirmation
// step already carries the exact apply SQL, and rejecting it is the one path
// through the extension that reaches the diff and then runs zero statements. Note
// that this is an inference from how the extension is written, not a contract it
// publishes — see the limits in the sqlplan package doc.
//
// A diff is cached server-side for a few minutes, keyed on a fingerprint of the
// live schema plus the project version. So a plan followed by a real deploy reuses
// the same computed diff, which is what makes "the plan is what applies" true in
// the strongest sense; and a database that changed in between invalidates the key.
func (i *Implementation) computeSchemaPlan(targets *runTargets, t planTarget) (string, string, error) {
	outputtools.PrintlnColoredErr("Computing the schema plan (nothing will be applied)...", outputtools.Blue)

	res, err := i.sqlPushRun(targets, t, func(extensionrun.StepPrompt) (extensionrun.StepDecision, error) {
		return extensionrun.StepDecision{Confirm: false, Reason: "dry run (--plan)"}, nil
	}, nil)
	// A cancelled execution is the expected outcome here: the rejection landed and
	// nothing ran. Anything else with an error is a real failure.
	if err != nil && !errors.Is(err, extensionrun.ErrExecutionCancelled) {
		return "", "", err
	}
	if res == nil {
		// Only reachable if the cancelled sentinel ever arrives without a result.
		// Better an honest error than a panic.
		return "", "", errors.New("the SQL-push extension returned no result, so there is no plan to show")
	}
	// Terminal success without any confirmation step is the extension's "no changes
	// to apply" short-circuit — there was no migration to show.
	return res.SQLPreview(), res.StatusMessage, nil
}

// lastGoCodeGenConfig returns the project's last-used go-code-gen config values, or
// nil. That config is where the deployment identifier lives when --identifier is
// not passed.
func lastGoCodeGenConfig(targets *runTargets) map[string]interface{} {
	if targets == nil || targets.allLastConfigs == nil {
		return nil
	}
	return targets.allLastConfigs[goCodeGenExtensionIdentifier].ConfigValues
}

func isMySQL(engine string) bool {
	return deploy.DBEngine(engine) == deploy.DBMySQL
}

// planProjectVersion renders a version for the report, including its review status.
// A plan may be computed from a draft, so every plan says which it was.
func planProjectVersion(pv *nemgen.ProjectVersion) planVersion {
	if pv == nil {
		return planVersion{}
	}
	status := pv.GetReviewStatus()
	return planVersion{
		UUID:         pv.GetUuid(),
		Identifier:   pv.GetIdentifier(),
		ReviewStatus: reviewStatusName(status),
		Approved: status == nemgen.ProjectVersionReviewStatus_PROJECT_VERSION_REVIEW_STATUS_APPROVED ||
			status == nemgen.ProjectVersionReviewStatus_PROJECT_VERSION_REVIEW_STATUS_PUBLISHED,
	}
}

// reviewStatusName is the enum as a short uppercase name, for the plan header and
// JSON. Distinct from reviewStatusLabel, which phrases a status as a problem to fix
// ("still a draft") because it is used in an error.
func reviewStatusName(s nemgen.ProjectVersionReviewStatus) string {
	switch s {
	case nemgen.ProjectVersionReviewStatus_PROJECT_VERSION_REVIEW_STATUS_DRAFT:
		return "DRAFT"
	case nemgen.ProjectVersionReviewStatus_PROJECT_VERSION_REVIEW_STATUS_IN_REVIEW:
		return "IN_REVIEW"
	case nemgen.ProjectVersionReviewStatus_PROJECT_VERSION_REVIEW_STATUS_APPROVED:
		return "APPROVED"
	case nemgen.ProjectVersionReviewStatus_PROJECT_VERSION_REVIEW_STATUS_REJECTED:
		return "REJECTED"
	case nemgen.ProjectVersionReviewStatus_PROJECT_VERSION_REVIEW_STATUS_PUBLISHED:
		return "PUBLISHED"
	default:
		return fmt.Sprintf("UNKNOWN(%d)", int32(s))
	}
}

// rerunCommand renders the command that applies what was just planned: the
// invocation the user actually typed, minus the plan-only flags, plus
// --allow-destructive when the plan needs it.
//
// Concrete beats a generic "pass --allow-destructive": the user can paste this. Which
// is precisely why the SELECTOR has to survive. --deployment used to be dropped as
// "meaningless outside a plan", so planning with `--plan --deployment r6box-c3d31228
// --version <v3>` suggested `nuzur-cli deploy --version <v3> --allow-destructive`:
// no project, no provider, no identifier, none of which the user had typed because
// the record carried them. Run verbatim that fails with "--host is required for the
// ssh provider" — or, worse, on a TTY prompts for a project and aims a command
// carrying --allow-destructive at a database nobody planned against. --deployment is
// now a selector for a real deploy too (see applyDeploymentSelector), so keeping it
// is what makes the suggestion target exactly what was planned.
func rerunCommand(argv []string, addAllowDestructive bool) string {
	if len(argv) == 0 {
		return "nuzur-cli deploy"
	}
	skip := map[string]bool{"--plan": true, "--json": true}
	// Flags that take a value and are genuinely plan-only; their value has to be
	// dropped along with the flag. Both name a database directly rather than through
	// a record, and a deploy has no use for either — it reaches its database through
	// the box it is deploying to.
	skipWithValue := map[string]bool{"--local-agent": true, "--local-agent-connection": true}

	out := []string{shellQuote(baseName(argv[0]))}
	hasAllow := false
	for idx := 1; idx < len(argv); idx++ {
		a := argv[idx]
		name, _, hasInlineValue := strings.Cut(a, "=")
		if skip[name] {
			continue
		}
		if skipWithValue[name] {
			if !hasInlineValue {
				idx++ // also drop the separated value
			}
			continue
		}
		if name == "--allow-destructive" {
			hasAllow = true
		}
		out = append(out, shellQuote(a))
	}
	if addAllowDestructive && !hasAllow {
		out = append(out, "--allow-destructive")
	}
	return strings.Join(out, " ")
}

// targetChosenByLocalAgentFlags reports whether the plan reached its database
// through --local-agent/--local-agent-connection — precedence 1 in
// resolvePlanTargetFromState, which wins outright whenever both are given.
//
// It is derived from the same input the resolution reads rather than from the
// resolved target's Source string, because Source is documented as reporting-only
// and this decides whether a command is handed to the user.
func targetChosenByLocalAgentFlags(in planTargetInput) bool {
	return strings.TrimSpace(in.LocalAgent) != "" && strings.TrimSpace(in.LocalAgentConn) != ""
}

// localAgentRerunNote replaces the pasteable command when the plan was aimed with
// the --local-agent pair.
//
// NO COMMAND IS OFFERED, deliberately. rerunCommand drops those two flags — a
// deploy really cannot use them, it reaches its database through the box it is
// deploying to — and what is left re-resolves through the host+identifier lookup
// to whatever connection the record for that box happens to hold. That is a
// DIFFERENT database from the one just diffed, and when the plan was destructive
// the suggestion carried --allow-destructive to it.
//
// Qualifying the command with a warning was the alternative and is worse: the
// value of the suggestion is that it can be pasted without being read, so a
// pasteable command plus a caveat is a pasteable command. The rule the suggestion
// exists to keep is that it applies to what was planned; where that cannot be
// promised, the honest output is the explanation and no command.
func localAgentRerunNote(connUUID string) string {
	which := "the connection this plan targeted"
	if c := strings.TrimSpace(connUUID); c != "" {
		which = "connection " + c
	}
	return "No re-run command is offered for this plan.\n" +
		"It was aimed with --local-agent/--local-agent-connection, and `deploy` accepts neither — a deploy\n" +
		"reaches its database through the box it deploys to. A command with those flags removed would\n" +
		"resolve to whatever connection the record for its host and identifier holds, which need not be\n" +
		which + " — so pasting it could apply this migration somewhere nobody diffed.\n" +
		"To apply it, re-plan with the selector you will deploy with (`--deployment <id>` for a box this\n" +
		"machine deployed, or the same --host/--identifier) and use the command that plan suggests."
}

// baseName is filepath.Base without importing filepath for one call, and keeps the
// rerun command readable when the CLI was invoked by absolute path.
func baseName(p string) string {
	if idx := strings.LastIndexAny(p, `/\`); idx >= 0 {
		return p[idx+1:]
	}
	return p
}

// shellQuote quotes an argument only when it needs it, so the common case stays
// copy-pasteable and readable.
func shellQuote(a string) string {
	if a != "" && !strings.ContainsAny(a, " \t\n\"'$`\\*?") {
		return a
	}
	return "'" + strings.ReplaceAll(a, "'", `'\''`) + "'"
}
