package app

import (
	"bytes"
	"strings"
	"testing"

	"github.com/nuzur/nuzur-cli/extensionrun"
)

var sqlStep = extensionrun.StepPrompt{StepIdentifier: "sql-validation", Title: "Review SQL", Content: "CREATE TABLE IF NOT EXISTS `codebase` (...);"}

func TestPreviewStepsPrintsAndDeclines(t *testing.T) {
	var out bytes.Buffer
	decide := stepDeciderForRun(extRunFlags{previewSteps: true, nonInteractive: true}, false, strings.NewReader(""), &out)
	got, err := decide(sqlStep)
	if err != nil || got.Confirm {
		t.Fatalf("preview must decline without error, got %+v %v", got, err)
	}
	if !strings.Contains(out.String(), "CREATE TABLE IF NOT EXISTS `codebase`") {
		t.Errorf("preview must print the step content, got %q", out.String())
	}
}

func TestInteractiveStepsAsk(t *testing.T) {
	for answer, want := range map[string]bool{"y\n": true, "yes\n": true, "\n": false, "n\n": false, "": false} {
		var out bytes.Buffer
		decide := stepDeciderForRun(extRunFlags{}, true, strings.NewReader(answer), &out)
		got, err := decide(sqlStep)
		if answer == "" {
			if err == nil {
				t.Error("EOF with no answer should be an error, not a silent decline")
			}
			continue
		}
		if err != nil || got.Confirm != want {
			t.Errorf("answer %q: got %+v %v, want confirm=%v", answer, got, err, want)
		}
		if !strings.Contains(out.String(), "Review SQL") {
			t.Errorf("the step must be shown before asking, got %q", out.String())
		}
	}
}

func TestStepDeciderDefersToAutoConfirm(t *testing.T) {
	if stepDeciderForRun(extRunFlags{confirmSteps: true}, true, nil, nil) != nil {
		t.Error("--confirm-steps uses RunParams.AutoConfirmSteps, not a custom decider")
	}
	if stepDeciderForRun(extRunFlags{nonInteractive: true}, false, nil, nil) != nil {
		t.Error("a non-interactive run with no step flag keeps the existing error behavior")
	}
}
