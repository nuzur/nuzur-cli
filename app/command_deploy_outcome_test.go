package app

import (
	"strings"
	"testing"

	"github.com/nuzur/nuzur-cli/outputtools"
)

// The two steps of deploy's step 10 used to share one boolean, so a failed catalog
// publish was reported as "Schema auto-apply skipped". These tests pin each outcome
// to a message about the subsystem that actually failed.

func TestDeployOutcomeSummary(t *testing.T) {
	tests := []struct {
		name string
		o    deployOutcome
		// wantMentions/wantAbsent are matched case-insensitively against the summary.
		wantMentions []string
		wantAbsent   []string
	}{
		{
			name:       "both succeeded says nothing",
			o:          deployOutcome{catalogPublished: true, schemaApplied: true},
			wantAbsent: []string{"schema", "connection"},
		},
		{
			name:         "publish failed blames the connection, not the schema",
			o:            deployOutcome{catalogPublished: false, schemaApplied: true},
			wantMentions: []string{"connection was NOT published", "Via agent"},
			wantAbsent:   []string{"schema was NOT applied"},
		},
		{
			name:         "schema failed blames the schema, not the connection",
			o:            deployOutcome{catalogPublished: true, schemaApplied: false},
			wantMentions: []string{"schema was NOT applied", "--plan"},
			wantAbsent:   []string{"connection was NOT published"},
		},
		{
			name: "a failed apply on a box that was already rebuilt says the app is mismatched",
			o:    deployOutcome{catalogPublished: true, schemaApplied: false, appShipped: true},
			wantMentions: []string{
				"does NOT match the database",
				"re-deploy the version that was running before this one",
			},
		},
		{
			// --db-only ships no app, so there is nothing to be mismatched.
			name:       "a db-only deploy is not told its app is mismatched",
			o:          deployOutcome{catalogPublished: true, schemaApplied: false},
			wantAbsent: []string{"rebuilt and restarted"},
		},
		{
			name:         "both failed reports both",
			o:            deployOutcome{catalogPublished: false, schemaApplied: false},
			wantMentions: []string{"connection was NOT published", "schema was NOT applied"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := tt.o.summary()
			if tt.o.catalogPublished && tt.o.schemaApplied && got != "" {
				t.Fatalf("clean deploy produced a warning: %q", got)
			}
			for _, want := range tt.wantMentions {
				if !strings.Contains(got, want) {
					t.Errorf("summary is missing %q\ngot: %s", want, got)
				}
			}
			for _, absent := range tt.wantAbsent {
				if strings.Contains(strings.ToLower(got), strings.ToLower(absent)) {
					t.Errorf("summary should not mention %q\ngot: %s", absent, got)
				}
			}
		})
	}
}

// The old block asserted a cause the code could not know, and told the user the
// agent connection was live in exactly the case where it was not.
func TestDeployOutcomeSummaryDoesNotGuessACause(t *testing.T) {
	for _, o := range []deployOutcome{
		{catalogPublished: false, schemaApplied: true},
		{catalogPublished: true, schemaApplied: false},
		{catalogPublished: false, schemaApplied: false},
	} {
		got := strings.ToLower(o.summary())
		if strings.Contains(got, "diff step") {
			t.Errorf("summary asserts a cause it cannot know: %s", got)
		}
		if !o.catalogPublished && strings.Contains(got, "agent connection are live") {
			t.Errorf("summary claims the connection is live when publishing failed: %s", got)
		}
	}
}

// The failed-apply branch used to end with "so the database is still empty" — text
// that only makes sense on a first deploy, emitted unconditionally. On a re-deploy it
// asserted something false about the user's data at exactly the moment a statement had
// errored against a database full of rows, and it read as reassurance.
func TestDeployOutcomeSummaryNeverClaimsTheDatabaseIsEmpty(t *testing.T) {
	for _, o := range []deployOutcome{
		{catalogPublished: true, schemaApplied: false},
		{catalogPublished: true, schemaApplied: false, appShipped: true},
		{catalogPublished: true, schemaApplied: false, schemaBlocked: true, destructiveCount: 1},
		{catalogPublished: false, schemaApplied: false},
	} {
		got := strings.ToLower(o.summary())
		for _, forbidden := range []string{"still empty", "database is empty", "no data"} {
			if strings.Contains(got, forbidden) {
				t.Errorf("summary asserts %q about a database it never read: %s", forbidden, got)
			}
		}
	}
}

// sql-push now requests a transaction, so a failed apply may or may not have rolled
// back. The summary may only claim a rollback it actually knows about — telling
// somebody their migration rolled back when it did not is how a half-applied schema
// goes unnoticed.
func TestDeployOutcomeSummaryOnlyClaimsAKnownRollback(t *testing.T) {
	rolled := deployOutcome{catalogPublished: true, schemaApplied: false, schemaRolledBack: true}.summary()
	if !strings.Contains(rolled, "rolled back") {
		t.Errorf("a known rollback should be reported: %s", rolled)
	}
	if strings.Contains(rolled, "Check the database before retrying") {
		t.Errorf("a known rollback should not send the user checking: %s", rolled)
	}

	unknown := deployOutcome{catalogPublished: true, schemaApplied: false}.summary()
	if strings.Contains(unknown, "rolled back and the database is as it was") {
		t.Errorf("an unknown outcome must not claim a rollback: %s", unknown)
	}
	if !strings.Contains(unknown, "Check the database before retrying") {
		t.Errorf("an unknown outcome should send the user checking: %s", unknown)
	}
	// Either way, retrying a deterministic failure is not the advice.
	for _, s := range []string{rolled, unknown} {
		if !strings.Contains(s, "--plan") {
			t.Errorf("summary should point at --plan: %s", s)
		}
	}
}

// An unapplied schema — blocked or failed — leaves the running app out of step with
// its database. Neither case is a yellow heads-up.
func TestDeployOutcomeSummaryColor(t *testing.T) {
	for _, tc := range []struct {
		name string
		o    deployOutcome
		want outputtools.OutputColor
	}{
		{name: "blocked", o: deployOutcome{schemaBlocked: true, schemaApplied: false}, want: outputtools.Red},
		{name: "failed", o: deployOutcome{schemaApplied: false}, want: outputtools.Red},
		{name: "publish failed only", o: deployOutcome{schemaApplied: true}, want: outputtools.Yellow},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.o.summaryColor(); got != tc.want {
				t.Fatalf("summaryColor() = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestDeployOutcomeRevisionMessage(t *testing.T) {
	tests := []struct {
		name string
		o    deployOutcome
		want string
	}{
		{
			name: "clean deploy records no shortfall",
			o:    deployOutcome{catalogPublished: true, schemaApplied: true},
			want: "",
		},
		{
			name: "publish failure",
			o:    deployOutcome{catalogPublished: false, schemaApplied: true},
			want: "connection not published to nuzur",
		},
		{
			name: "schema failure",
			o:    deployOutcome{catalogPublished: true, schemaApplied: false},
			want: "schema not applied to the database",
		},
		{
			name: "both",
			o:    deployOutcome{catalogPublished: false, schemaApplied: false},
			want: "connection not published to nuzur; schema not applied to the database",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.o.revisionMessage(); got != tt.want {
				t.Errorf("revisionMessage() = %q, want %q", got, tt.want)
			}
		})
	}
}

// The revision status message is stored in a varchar(512) and truncated past that.
func TestDeployOutcomeRevisionMessageFitsTheColumn(t *testing.T) {
	worst := deployOutcome{catalogPublished: false, schemaApplied: false}.revisionMessage()
	if len(worst) > maxStatusMessage {
		t.Errorf("worst-case revision message is %d chars, exceeds the %d cap", len(worst), maxStatusMessage)
	}
}
