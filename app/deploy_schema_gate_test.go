package app

import (
	"errors"
	"strings"
	"testing"

	"github.com/nuzur/nuzur-cli/outputtools"
	"github.com/nuzur/nuzur-cli/sqlplan"
	"github.com/urfave/cli"
)

func TestDecideSchemaApply(t *testing.T) {
	for _, tc := range []struct {
		name             string
		applySQL         string
		allowDestructive bool
		wantConfirm      bool
	}{
		{name: "empty plan applies", applySQL: "", wantConfirm: true},
		{
			name:        "additive only applies",
			applySQL:    "CREATE TABLE a (id INT);\nALTER TABLE b ADD COLUMN c INT;",
			wantConfirm: true,
		},
		{
			// Pinned deliberately. Column redefinitions are where MySQL's
			// reconstructed-side churn lands, and they are also ordinary schema
			// evolution. A gate that fired on these would block almost every deploy
			// and teach people to pass --allow-destructive by reflex.
			name:        "narrowing only applies",
			applySQL:    "ALTER TABLE `a` MODIFY COLUMN `x` VARCHAR(512);\nALTER TABLE b ALTER COLUMN y SET NOT NULL;",
			wantConfirm: true,
		},
		{
			// Dropping an index loses a guarantee, not data. Rebuildable.
			name:        "constraint loss only applies",
			applySQL:    "DROP INDEX idx_a;\nALTER TABLE b DROP CONSTRAINT fk_b;",
			wantConfirm: true,
		},
		{
			name:        "data loss without the flag is refused",
			applySQL:    "DROP TABLE audit_2023;",
			wantConfirm: false,
		},
		{
			name:             "data loss with the flag applies",
			applySQL:         "DROP TABLE audit_2023;",
			allowDestructive: true,
			wantConfirm:      true,
		},
		{
			name:        "a column drop hidden in a multi-action ALTER is still refused",
			applySQL:    "ALTER TABLE t ADD COLUMN a INT, DROP COLUMN b;",
			wantConfirm: false,
		},
		{
			// The additive statements do not get applied either: the migration goes to
			// the database as one unit, so refusing it refuses all of it.
			name:        "a mixed plan with any data loss is refused whole",
			applySQL:    "CREATE TABLE a (id INT);\nDROP TABLE b;\nCREATE INDEX i ON a (id);",
			wantConfirm: false,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			plan := sqlplan.Analyze(tc.applySQL)
			confirm, reason := decideSchemaApply(plan, tc.allowDestructive)
			if confirm != tc.wantConfirm {
				t.Fatalf("confirm = %v, want %v (reason: %s)", confirm, tc.wantConfirm, reason)
			}
			if strings.TrimSpace(reason) == "" {
				t.Fatal("a decision with no reason cannot be reported")
			}
			// A refusal has to name the flag, or the user cannot act on it.
			if !confirm && !strings.Contains(reason, "--allow-destructive") {
				t.Fatalf("refusal reason does not name the flag: %q", reason)
			}
		})
	}
}

func TestRevisionShouldFail(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
		want bool
	}{
		{name: "success", err: nil, want: false},
		{name: "a real error fails the revision", err: errors.New("ssh: connection refused"), want: true},
		{
			// The box IS provisioned and serving; the revision message already records
			// what was skipped. Marking it FAILED would claim the deploy did not happen.
			name: "a bare exit error does not fail the revision",
			err:  cli.NewExitError("", 1),
			want: false,
		},
		{name: "the blocked-schema sentinel is a real error on its own", err: errSchemaBlocked, want: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := revisionShouldFail(tc.err); got != tc.want {
				t.Fatalf("revisionShouldFail(%v) = %v, want %v", tc.err, got, tc.want)
			}
		})
	}
}

func TestExitCodeForOutcome(t *testing.T) {
	if err := exitCodeForOutcome(deployOutcome{catalogPublished: true, schemaApplied: true}); err != nil {
		t.Fatalf("a clean deploy returned %v", err)
	}
	// A schema that failed for an ordinary reason keeps the historical behavior:
	// warn, exit zero. Only a deliberate block changes the exit code.
	if err := exitCodeForOutcome(deployOutcome{schemaApplied: false}); err != nil {
		t.Fatalf("a failed-but-not-blocked schema returned %v", err)
	}
	err := exitCodeForOutcome(deployOutcome{schemaBlocked: true})
	if err == nil {
		t.Fatal("a blocked schema exited zero")
	}
	exit, ok := err.(*cli.ExitError)
	if !ok {
		t.Fatalf("got %T, want *cli.ExitError", err)
	}
	if exit.ExitCode() != 1 {
		t.Fatalf("exit code = %d, want 1", exit.ExitCode())
	}
}

func TestDeployOutcomeBlockedSummary(t *testing.T) {
	o := deployOutcome{
		catalogPublished:   true,
		schemaApplied:      false,
		schemaBlocked:      true,
		destructiveCount:   2,
		rerunCommand:       "nuzur-cli deploy --host prod --allow-destructive",
		destructiveApplied: false,
	}
	s := o.summary()
	for _, want := range []string{
		"2 statements",
		"DELETE DATA",
		"--allow-destructive",
		"nuzur-cli deploy --host prod --allow-destructive",
		"--plan",
		"OLD schema",
	} {
		if !strings.Contains(s, want) {
			t.Errorf("summary() missing %q:\n%s", want, s)
		}
	}
	// Retrying does nothing — the deploy needs a decision, not another attempt.
	if strings.Contains(s, "Re-run the deploy to retry") {
		t.Errorf("blocked summary tells the user to retry:\n%s", s)
	}
	if got := o.summaryColor(); got != outputtools.Red {
		t.Errorf("summaryColor() = %v, want Red", got)
	}
}

func TestDeployOutcomeRevisionMessages(t *testing.T) {
	for _, tc := range []struct {
		name string
		o    deployOutcome
		want string
	}{
		{
			name: "clean deploy says nothing",
			o:    deployOutcome{catalogPublished: true, schemaApplied: true},
			want: "",
		},
		{
			name: "blocked names the flag and the count",
			o:    deployOutcome{catalogPublished: true, schemaBlocked: true, destructiveCount: 2},
			want: "schema not applied: 2 destructive statement(s) need --allow-destructive",
		},
		{
			name: "an authorized drop is legible in the history",
			o:    deployOutcome{catalogPublished: true, schemaApplied: true, destructiveApplied: true, destructiveCount: 3},
			want: "schema applied including 3 destructive statement(s)",
		},
		{
			name: "an ordinary schema failure is unchanged",
			o:    deployOutcome{catalogPublished: true, schemaApplied: false},
			want: "schema not applied to the database",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.o.revisionMessage(); got != tc.want {
				t.Fatalf("revisionMessage() = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestDeployOutcomeRevisionMessageFitsTheColumnWithGateFields(t *testing.T) {
	// The revision message column is a varchar(512). This is the new worst case: a
	// failed publish AND a blocked destructive schema.
	o := deployOutcome{
		catalogPublished: false,
		schemaApplied:    false,
		schemaBlocked:    true,
		destructiveCount: 999999,
	}
	if got := len(o.revisionMessage()); got > 512 {
		t.Fatalf("revisionMessage() is %d chars, over the 512-char column", got)
	}
}
