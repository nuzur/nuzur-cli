package app

import "strings"

// deployOutcome records what step 10 of a deploy achieved: publishing the box's
// database as an agent connection in nuzur, and applying the project's schema to it.
//
// The two are tracked separately on purpose. They used to share one boolean and one
// error path, so a failure to publish the connection was reported as "Schema
// auto-apply skipped" — a message about the wrong subsystem, which is why an empty
// connection catalog went unnoticed until it surfaced as an unrelated UI error.
type deployOutcome struct {
	catalogPublished bool
	schemaApplied    bool
}

// summary is the closing warning printed after the deployment report, or "" when
// both steps succeeded. It names only what actually failed and what that costs the
// user, and never guesses at a cause — the specific error was already printed at the
// point of failure.
func (o deployOutcome) summary() string {
	var parts []string
	if !o.catalogPublished {
		parts = append(parts,
			"The connection was NOT published to nuzur (see the error above), so this database will not appear "+
				"in the data manager under \"Via agent\". The database and the agent are running on the box — "+
				"re-run the deploy to retry publishing it.")
	}
	if !o.schemaApplied {
		parts = append(parts,
			"The schema was NOT applied (see the error above), so the database is still empty. "+
				"Re-run the deploy to retry, or apply the schema from nuzur (SQL Push / change request).")
	}
	return strings.Join(parts, "\n\n")
}

// revisionMessage is recorded on the ACTIVE deployment revision in nuzur. It is
// deliberately terse: the field is a varchar(512) and the CLI truncates past that.
// Empty when nothing was skipped, so a clean deploy still reports a clean revision.
func (o deployOutcome) revisionMessage() string {
	var parts []string
	if !o.catalogPublished {
		parts = append(parts, "connection not published to nuzur")
	}
	if !o.schemaApplied {
		parts = append(parts, "schema not applied to the database")
	}
	return strings.Join(parts, "; ")
}
