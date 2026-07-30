package app

import (
	"fmt"
	"strings"

	"github.com/nuzur/nuzur-cli/outputtools"
)

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
	// schemaBlocked marks the one case where the schema was not applied ON PURPOSE:
	// the migration deleted data and nobody authorized that. It is not a failure to
	// retry — retrying changes nothing — so it gets its own message, its own color
	// and its own revision text.
	schemaBlocked bool
	// destructiveCount is how many statements would delete (or did delete) data.
	destructiveCount int
	// rerunCommand is the exact invocation that would apply the blocked migration.
	rerunCommand string
	// destructiveApplied records that this deploy deleted data with authorization, so
	// the deployment history in nuzur says so rather than reading as a clean deploy.
	destructiveApplied bool
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
	switch {
	case o.schemaBlocked:
		// Deliberately NOT "re-run the deploy to retry": re-running changes nothing.
		// This needs a decision, and the user has to know both what it costs and
		// what state the box is in until they make it.
		msg := fmt.Sprintf(
			"The schema was NOT applied: %s would DELETE DATA, and --allow-destructive was not passed. "+
				"Nothing was changed in the database — not the destructive statements and not the rest of "+
				"the migration, because they are applied as one unit.",
			plural(o.destructiveCount, "statement"))
		if o.rerunCommand != "" {
			msg += "\n\nTo apply it, re-run with authorization:\n  " + o.rerunCommand
		}
		msg += "\n\nUntil then the app on this box is serving against the OLD schema, which its " +
			"generated code no longer matches. Run the same command with --plan to read the full " +
			"migration first — a plan applies nothing."
		parts = append(parts, msg)
	case !o.schemaApplied:
		parts = append(parts,
			"The schema was NOT applied (see the error above), so the database is still empty. "+
				"Re-run the deploy to retry, or apply the schema from nuzur (SQL Push / change request).")
	}
	return strings.Join(parts, "\n\n")
}

// summaryColor is how loudly the closing summary should be printed. A blocked schema
// is the one deploy shortfall where the running app's generated code does not match
// the database it is talking to, so it is not a yellow "heads up".
func (o deployOutcome) summaryColor() outputtools.OutputColor {
	if o.schemaBlocked {
		return outputtools.Red
	}
	return outputtools.Yellow
}

// plural renders a count with its noun.
func plural(n int, noun string) string {
	if n == 1 {
		return fmt.Sprintf("1 %s", noun)
	}
	return fmt.Sprintf("%d %ss", n, noun)
}

// revisionMessage is recorded on the ACTIVE deployment revision in nuzur. It is
// deliberately terse: the field is a varchar(512) and the CLI truncates past that.
// Empty when nothing was skipped, so a clean deploy still reports a clean revision.
func (o deployOutcome) revisionMessage() string {
	var parts []string
	if !o.catalogPublished {
		parts = append(parts, "connection not published to nuzur")
	}
	switch {
	case o.schemaBlocked:
		parts = append(parts, fmt.Sprintf("schema not applied: %d destructive statement(s) need --allow-destructive", o.destructiveCount))
	case !o.schemaApplied:
		parts = append(parts, "schema not applied to the database")
	case o.destructiveApplied:
		// A deploy that dropped data on purpose should be legible as such in the
		// history, not just as a successful deploy.
		parts = append(parts, fmt.Sprintf("schema applied including %d destructive statement(s)", o.destructiveCount))
	}
	return strings.Join(parts, "; ")
}
