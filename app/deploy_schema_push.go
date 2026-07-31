package app

import (
	"fmt"
	"os"

	"github.com/nuzur/nuzur-cli/deploy"
	"github.com/nuzur/nuzur-cli/extensionrun"
	"github.com/nuzur/nuzur-cli/outputtools"
)

// deploy_schema_push.go is the single place that knows how deploy talks to the
// SQL-push extension.
//
// Applying a schema and planning one are the same conversation with the extension,
// differing only in how the CLI answers its confirmation step. Keeping them in one
// function is not a tidiness preference: it is the reason a plan can be trusted.
// If the plan path built its own request, "the SQL you were shown" and "the SQL
// that runs" would be two separately-maintained answers that agree until the day
// they don't.

// planTarget is the live database a schema push or plan is aimed at, in the two
// shapes sql-push understands: through a box's local agent, or directly at a nuzur
// team connection.
type planTarget struct {
	// Mode picks the member of the sql-push pair, and is derived from the
	// deployment's topology rather than configured. A database behind a box is only
	// reachable through that box's agent; a team connection is reachable from nuzur
	// directly, and pushing to it remotely keeps it in sync the same way any other
	// schema change to it would.
	Mode connectionMode

	AgentUUID string // Mode == connModeLocal
	ConnUUID  string // Mode == connModeLocal

	TeamConnUUID string // Mode == connModeRemote
	TeamStore    string // Mode == connModeRemote

	Schema string
	Engine deploy.DBEngine

	// DeploymentID and Source are for reporting only: Source names how this target
	// was chosen, so a plan's header can say what it is planning against.
	DeploymentID string
	Source       string
}

// configValues renders the target as the config the chosen sql-push member takes.
func (t planTarget) configValues() map[string]interface{} {
	if t.Mode == connModeRemote {
		return map[string]interface{}{
			"store":      t.TeamStore,
			"connection": t.TeamConnUUID,
			"schema":     t.Schema,
		}
	}
	return map[string]interface{}{
		"local_agent":            t.AgentUUID,
		"local_agent_connection": t.ConnUUID,
		"local_agent_schema":     t.Schema,
	}
}

// extensionIdentifier picks the sql-push member this target requires.
func (t planTarget) extensionIdentifier() string {
	if t.Mode == connModeRemote {
		return sqlPushPair.Front
	}
	return sqlPushPair.Local
}

// sqlPushRun runs the SQL-push extension against a target, letting the caller
// decide the confirmation step.
//
// The decider is the entire difference between applying a schema and planning one.
func (i *Implementation) sqlPushRun(targets *runTargets, t planTarget, decide extensionrun.StepDecider) (*extensionrun.RunResult, error) {
	extID := t.extensionIdentifier()
	ext, err := targets.er.FindExtensionByIdentifier(extID)
	if err != nil {
		return nil, fmt.Errorf("resolving %q: %w", extID, err)
	}
	ver, err := targets.er.GetLatestExtensionVersion(ext.Uuid)
	if err != nil {
		return nil, err
	}
	// sql-push returns no downloadable file, so this directory stays empty. It
	// exists because Run requires an output path.
	outDir, err := os.MkdirTemp("", "nuzur-sqlpush-*")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(outDir)

	return targets.er.Run(extensionrun.RunParams{
		Extension:          ext,
		ExtensionVersion:   ver,
		ProjectUUID:        targets.project.Uuid,
		ProjectVersionUUID: targets.projectVersion.Uuid,
		ConfigValues:       t.configValues(),
		OnConfirmationStep: decide,
		OutputPath:         outDir,
	})
}

// sqlGenExtensionIdentifier renders a project version's DDL without touching any
// database.
const sqlGenExtensionIdentifier = "sql-gen"

// computeCreatePlan renders the CREATE script a first deploy would run, for the
// case where there is no live database to diff against.
//
// Answering "there is no database, so I cannot tell you anything" would be a worse
// product than answering "here is everything it would create", and the second answer
// is one read-only generator run away.
func (i *Implementation) computeCreatePlan(targets *runTargets, engine deploy.DBEngine) (string, error) {
	// The resolved project version does not carry entities (it is fetched with
	// ExcludeJsonFields), so pull the full schema here rather than reading it off
	// the object we already have — counting entities on the stripped object made
	// this path report "no standalone entities" for every project that has some.
	// Dependent entities are part of their parent's table, so the DDL generator
	// ignores them; GetStandaloneEntities already filters to the ones that become
	// tables.
	standalone, err := targets.er.GetStandaloneEntities(targets.projectVersion.GetUuid())
	if err != nil {
		return "", fmt.Errorf("loading the schema of project version %s: %w", targets.projectVersion.GetIdentifier(), err)
	}
	var entities []string
	for _, e := range standalone {
		if e.GetIdentifier() != "" {
			entities = append(entities, e.GetUuid())
		}
	}
	if len(entities) == 0 {
		// sql-gen renders only the entities it is handed, so an empty list would
		// come back as an empty script — indistinguishable from "no changes". Say
		// what actually happened instead.
		return "", fmt.Errorf("project version %s has no standalone entities, so there is no schema to create", targets.projectVersion.GetIdentifier())
	}

	ext, err := targets.er.FindExtensionByIdentifier(sqlGenExtensionIdentifier)
	if err != nil {
		return "", fmt.Errorf("resolving %q: %w", sqlGenExtensionIdentifier, err)
	}
	ver, err := targets.er.GetLatestExtensionVersion(ext.Uuid)
	if err != nil {
		return "", err
	}
	// sql-gen DOES return a zip, which Run downloads and extracts. It is thrown
	// away: the rendered SQL also comes back as a display block, which is what we
	// want, and a plan must not leave files behind.
	outDir, err := os.MkdirTemp("", "nuzur-sqlgen-*")
	if err != nil {
		return "", err
	}
	defer os.RemoveAll(outDir)

	outputtools.PrintlnColoredErr("Rendering the CREATE script for a first deploy...", outputtools.Blue)
	res, err := targets.er.Run(extensionrun.RunParams{
		Extension:          ext,
		ExtensionVersion:   ver,
		ProjectUUID:        targets.project.Uuid,
		ProjectVersionUUID: targets.projectVersion.Uuid,
		ConfigValues: map[string]interface{}{
			"db_type":  string(engine),
			"entities": entities,
			"actions":  []string{"create"},
		},
		OutputPath: outDir,
	})
	if err != nil {
		return "", err
	}
	// sql-gen names each block after the action that produced it.
	if block := res.DisplayBlock("create"); block != nil {
		return block.Content, nil
	}
	return res.SQLPreview(), nil
}
