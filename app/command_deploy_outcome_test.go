package app

import (
	"strings"
	"testing"
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
			wantMentions: []string{"schema was NOT applied", "SQL Push"},
			wantAbsent:   []string{"connection was NOT published"},
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
