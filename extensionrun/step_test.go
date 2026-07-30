package extensionrun

import (
	"strings"
	"testing"

	extensiongen "github.com/nuzur/extension-sdk/idl/gen"
)

func TestStepDeciderPrecedence(t *testing.T) {
	t.Run("caller decider wins over AutoConfirmSteps", func(t *testing.T) {
		// AutoConfirmSteps is true AND a decider is set: the decider must win, or
		// the deploy gate could never reject anything.
		p := RunParams{
			AutoConfirmSteps: true,
			OnConfirmationStep: func(StepPrompt) (StepDecision, error) {
				return StepDecision{Confirm: false, Reason: "dry run"}, nil
			},
		}
		got, err := p.stepDecider()(StepPrompt{StepIdentifier: "sql-validation"})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if got.Confirm {
			t.Fatal("the caller's decider was ignored")
		}
		if got.Reason != "dry run" {
			t.Fatalf("reason = %q, want %q", got.Reason, "dry run")
		}
	})

	t.Run("AutoConfirmSteps confirms", func(t *testing.T) {
		got, err := RunParams{AutoConfirmSteps: true}.stepDecider()(StepPrompt{StepIdentifier: "s"})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if !got.Confirm {
			t.Fatal("AutoConfirmSteps did not confirm")
		}
	})

	t.Run("neither set is an error with the pre-existing message", func(t *testing.T) {
		// This message is the contract `run-extension` without --confirm-steps
		// relies on. It must not drift while the seam is added around it.
		_, err := RunParams{}.stepDecider()(StepPrompt{StepIdentifier: "sql-validation"})
		if err == nil {
			t.Fatal("expected an error")
		}
		for _, want := range []string{`confirmation step "sql-validation"`, "non-interactive", "auto-confirmation"} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("error = %q, missing %q", err.Error(), want)
			}
		}
	})
}

func TestStepPromptFromStep(t *testing.T) {
	t.Run("nil step", func(t *testing.T) {
		if got := stepPromptFromStep(nil); got != (StepPrompt{}) {
			t.Fatalf("got %+v, want the zero value", got)
		}
	})

	t.Run("step with no display block", func(t *testing.T) {
		// A bare confirmation with nothing to show must not panic — the decider
		// still has to be asked.
		got := stepPromptFromStep(&extensiongen.ExecutionResponseTypeStepData{StepIdentifier: "s"})
		if got.StepIdentifier != "s" {
			t.Fatalf("StepIdentifier = %q", got.StepIdentifier)
		}
		if got.Content != "" {
			t.Fatalf("Content = %q, want empty", got.Content)
		}
	})

	t.Run("full block", func(t *testing.T) {
		got := stepPromptFromStep(&extensiongen.ExecutionResponseTypeStepData{
			StepIdentifier: "sql-validation",
			DisplayBlock: &extensiongen.ExecutionResponseDisplayBlock{
				Identifier:  "sql-diff",
				Title:       "Review",
				Description: "changes to apply",
				Content:     "DROP TABLE a;",
				ContentType: extensiongen.DisplayBlockContentType_DISPLAY_BLOCK_CONTENT_TYPE_SQL,
			},
		})
		want := StepPrompt{
			StepIdentifier:  "sql-validation",
			BlockIdentifier: "sql-diff",
			Title:           "Review",
			Description:     "changes to apply",
			ContentType:     "sql",
			Content:         "DROP TABLE a;",
		}
		if got != want {
			t.Fatalf("got %+v, want %+v", got, want)
		}
	})
}

func TestContentTypeName(t *testing.T) {
	for _, tc := range []struct {
		in   extensiongen.DisplayBlockContentType
		want string
	}{
		{extensiongen.DisplayBlockContentType_DISPLAY_BLOCK_CONTENT_TYPE_SQL, "sql"},
		{extensiongen.DisplayBlockContentType_DISPLAY_BLOCK_CONTENT_TYPE_JSON, "json"},
		{extensiongen.DisplayBlockContentType_DISPLAY_BLOCK_CONTENT_TYPE_PLAIN_TEXT, "text"},
		{extensiongen.DisplayBlockContentType_DISPLAY_BLOCK_CONTENT_TYPE_GOLANG, "golang"},
		// An unrecognized type must not be silently reported as plain text.
		{extensiongen.DisplayBlockContentType(99), "unknown(99)"},
	} {
		if got := contentTypeName(tc.in); got != tc.want {
			t.Errorf("contentTypeName(%v) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestDisplayBlocksFrom(t *testing.T) {
	got := displayBlocksFrom([]*extensiongen.ExecutionResponseDisplayBlock{
		nil, // must be skipped, not panic
		{Identifier: "create", Content: "CREATE TABLE a (id INT);", ContentType: extensiongen.DisplayBlockContentType_DISPLAY_BLOCK_CONTENT_TYPE_SQL},
	})
	if len(got) != 1 {
		t.Fatalf("got %d blocks, want 1", len(got))
	}
	if got[0].Identifier != "create" || got[0].ContentType != "sql" {
		t.Fatalf("got %+v", got[0])
	}
	if displayBlocksFrom(nil) != nil {
		t.Fatal("displayBlocksFrom(nil) should be nil")
	}
}

func TestRunResultSQLPreview(t *testing.T) {
	sqlStep := StepOutcome{Prompt: StepPrompt{ContentType: "sql", Content: "-- from a step"}}
	sqlBlock := DisplayBlock{Identifier: "create", ContentType: "sql", Content: "-- from a block"}

	for _, tc := range []struct {
		name string
		r    *RunResult
		want string
	}{
		{name: "nil result", r: nil, want: ""},
		{name: "empty result", r: &RunResult{}, want: ""},
		{name: "from a confirmation step", r: &RunResult{Steps: []StepOutcome{sqlStep}}, want: "-- from a step"},
		{name: "from a terminal block", r: &RunResult{DisplayBlocks: []DisplayBlock{sqlBlock}}, want: "-- from a block"},
		{
			// The preference matters for the day sql-push gains a real dry-run mode
			// and returns the migration as a terminal block instead of a step.
			name: "a terminal block wins over a step",
			r:    &RunResult{Steps: []StepOutcome{sqlStep}, DisplayBlocks: []DisplayBlock{sqlBlock}},
			want: "-- from a block",
		},
		{
			name: "a non-sql block is not mistaken for the migration",
			r:    &RunResult{DisplayBlocks: []DisplayBlock{{ContentType: "json", Content: "{}"}}},
			want: "",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.r.SQLPreview(); got != tc.want {
				t.Fatalf("SQLPreview() = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestRunResultDisplayBlock(t *testing.T) {
	r := &RunResult{DisplayBlocks: []DisplayBlock{
		{Identifier: "insert", Content: "a"},
		{Identifier: "create", Content: "b"},
	}}
	got := r.DisplayBlock("create")
	if got == nil || got.Content != "b" {
		t.Fatalf("DisplayBlock(\"create\") = %+v", got)
	}
	if r.DisplayBlock("nope") != nil {
		t.Fatal("expected nil for a missing identifier")
	}
	if (*RunResult)(nil).DisplayBlock("create") != nil {
		t.Fatal("expected nil for a nil result")
	}
}
