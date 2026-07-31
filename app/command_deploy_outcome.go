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
	// appShipped records that this deploy rebuilt and restarted the application on the
	// box before the schema step ran. It gates the mismatch warning: "the app is now
	// serving code the database does not match" is only true when there is an app, and
	// a --db-only deploy has none.
	appShipped bool
	// schemaRolledBack records that the attempted migration was applied as ONE
	// transaction, so a failure took the whole thing back with it.
	//
	// Only true when both halves are known: the engine gives real atomicity
	// (Postgres, and only when the batch contains nothing it must run outside a
	// transaction) AND the plan that was attempted is in hand. It defaults false so
	// that not knowing produces the conservative message — "go and check the
	// database" is cheap when it turns out to be intact, and telling somebody their
	// migration rolled back when it did not is how a half-applied schema goes
	// unnoticed.
	schemaRolledBack bool
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
				"the migration, because when the gate fires the deploy sends the database nothing at all.",
			plural(o.destructiveCount, "statement"))
		if o.rerunCommand != "" {
			msg += "\n\nTo apply it, re-run with authorization:\n  " + o.rerunCommand
		}
		msg += "\n\nRun the same command with --plan to read the full migration first — a plan " +
			"applies nothing."
		msg += o.mismatchWarning()
		parts = append(parts, msg)
	case !o.schemaApplied:
		// Deliberately says NOTHING about what the database contains. This used to
		// claim "so the database is still empty", which is first-deploy wording that
		// was emitted unconditionally: on a re-deploy it was simply false, and it read
		// as "nothing to worry about, there was no data anyway" at exactly the moment a
		// statement had errored against a database full of rows.
		msg := "The schema was NOT applied (see the error above). "
		if o.schemaRolledBack {
			msg += "It was sent as one transaction, so the whole migration rolled back and the " +
				"database is as it was. "
		} else {
			msg += "The deploy cannot tell you what state the database is in: this migration was " +
				"not applied as one unit, so a statement that failed partway through leaves the " +
				"ones before it applied. Check the database before retrying. "
		}
		msg += "Read the migration with --plan first — a plan applies nothing. If the failure is " +
			"deterministic (a cast the engine rejects, a constraint the existing rows violate), " +
			"re-running the deploy reproduces it: fix the data or the model instead."
		msg += o.mismatchWarning()
		parts = append(parts, msg)
	}
	return strings.Join(parts, "\n\n")
}

// mismatchWarning is what not applying the schema costs, when the app was shipped
// first: the image is rebuilt and the container restarted before the schema step
// runs, so an unapplied migration leaves generated code talking to a database it was
// not generated from. The gate path used to be the only one that said so, which made
// the quieter and more dangerous path — an apply that actually errored — the one that
// warned about nothing.
//
// Empty when no app shipped (--db-only, or a re-deploy stopped by the pre-flight gate
// before the bootstrap ran), because then there is nothing mismatched to warn about.
func (o deployOutcome) mismatchWarning() string {
	if !o.appShipped {
		return ""
	}
	return "\n\nThe app on this box was already rebuilt and restarted, so it is now serving generated " +
		"code that does NOT match the database — every endpoint whose entity changed will fail. To " +
		"restore service without applying the migration, re-deploy the version that was running " +
		"before this one (`nuzur-cli deploy list` names the deployment; its revision history in nuzur " +
		"names the version)."
}

// summaryColor is how loudly the closing summary should be printed. Any deploy that
// did not get its schema applied leaves the running app's generated code out of step
// with the database it is talking to, so neither the blocked case nor the failed one
// is a yellow "heads up".
func (o deployOutcome) summaryColor() outputtools.OutputColor {
	if o.schemaBlocked || !o.schemaApplied {
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
