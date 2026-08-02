package app

import (
	"fmt"
	"strings"

	"github.com/nuzur/nuzur-cli/outputtools"
)

// schemaOutcomeState is what happened to the schema in step 10 — exactly one of four
// states, because they are mutually exclusive and the messages, the color, the revision
// text and the exit code all branch on which one it is.
//
// It replaces three independent booleans (applied/blocked/neverStarted) that could
// express eight combinations, of which four were meaningless and two were reachable
// only by mistake. Every branch that read them had to re-derive the state, and
// re-derive it identically, from a set the type did not constrain.
//
// The zero value is deliberately schemaStateFailedDuringApply, the conservative half:
// an outcome nobody set claims failure and sends the user to check the database rather
// than printing "Deployment complete." and exiting 0 on a deploy whose schema step was
// never classified. This mirrors what the booleans it replaces defaulted to
// (schemaApplied false, schemaNeverStarted false).
type schemaOutcomeState int

const (
	// schemaStateFailedDuringApply: SQL reached the database and something errored.
	// The only state in which the database may be half-migrated, and therefore the
	// only one for which schemaRolledBack means anything.
	schemaStateFailedDuringApply schemaOutcomeState = iota
	// schemaStateApplied: the migration was applied.
	schemaStateApplied
	// schemaStateBlocked marks the one case where the schema was not applied ON
	// PURPOSE: the migration deleted data and nobody authorized that. It is not a
	// failure to retry — retrying changes nothing — so it gets its own message, its
	// own color and its own revision text.
	schemaStateBlocked
	// schemaStateFailedBeforeSQL: the apply failed BEFORE any SQL was sent —
	// resolving the sql-push extension, reaching the database, or computing the diff.
	//
	// It separates two states that were reported identically. A DeadlineExceeded while
	// RESOLVING "sql-push-local" produced "a statement that failed partway through
	// leaves the ones before it applied. Check the database before retrying" and "the
	// app is now serving generated code that does NOT match the database" — of a run
	// in which nothing had been sent. Both were false: the plan 30 seconds later was
	// empty and every endpoint answered 200, while the user was sent to audit a
	// database that had never been touched.
	schemaStateFailedBeforeSQL
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
	// schema is which of the four terminal states the schema step ended in. Not
	// knowing (an apply that was never classified) reads as failedDuringApply — see
	// schemaOutcomeState.
	schema schemaOutcomeState
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
	// An evidence bit for schemaStateFailedDuringApply and meaningful only there: it
	// is the one state in which SQL reached the database, so it is the only one whose
	// message has anything to roll back. The other three states leave it false.
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
	switch o.schema {
	case schemaStateBlocked:
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
	case schemaStateFailedBeforeSQL:
		// The apply never reached the database: the failure was in resolving the
		// sql-push extension, reaching the box's agent, or computing the diff. Nothing
		// was sent, so there is nothing to audit and nothing to be mismatched — and
		// saying otherwise is not a harmless over-warning. It sends the user to inspect
		// a database that is fine, and it puts "your app is serving code that does not
		// match its schema" on screen at the end of a deploy that changed neither.
		parts = append(parts,
			"The schema was NOT applied (see the error above). The failure happened before the "+
				"migration was sent — while resolving the SQL-push extension, reaching the database, "+
				"or computing the diff. No SQL was sent to the database. The app and database are "+
				"unchanged and consistent; re-run the deploy or apply via SQL Push.")
	case schemaStateFailedDuringApply:
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

// summaryColor is how loudly the closing summary should be printed. A deploy that
// did not get its schema applied exits non-zero and needs a decision from someone,
// so neither the blocked case nor the failed one is a yellow "heads up" — including
// the never-started one, where the database is intact but the migration still has
// not happened.
func (o deployOutcome) summaryColor() outputtools.OutputColor {
	if o.schema != schemaStateApplied {
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
	switch o.schema {
	case schemaStateBlocked:
		parts = append(parts, fmt.Sprintf("schema not applied: %d destructive statement(s) need --allow-destructive", o.destructiveCount))
	case schemaStateFailedBeforeSQL:
		// The revision history has to be able to tell these apart too: one says the
		// database may need inspecting, this one says it was never touched.
		parts = append(parts, "schema not applied: the apply failed before any SQL was sent")
	case schemaStateFailedDuringApply:
		parts = append(parts, "schema not applied to the database")
	case schemaStateApplied:
		if o.destructiveApplied {
			// A deploy that dropped data on purpose should be legible as such in the
			// history, not just as a successful deploy.
			parts = append(parts, fmt.Sprintf("schema applied including %d destructive statement(s)", o.destructiveCount))
		}
	}
	return strings.Join(parts, "; ")
}
