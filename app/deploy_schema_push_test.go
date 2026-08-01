package app

import (
	"errors"
	"testing"

	"github.com/nuzur/nuzur-cli/extensionrun"
)

// The confirmation step is the boundary between "nothing was sent" and "statements
// reached the database", and the deploy's closing summary says something different
// on each side of it. Everything before the step — resolving the sql-push extension,
// reaching the box's agent, computing the diff — leaves the database untouched; a
// failure there was being reported as a migration that died partway through.
func TestSQLPushProgressTracksTheConfirmationStep(t *testing.T) {
	for _, tc := range []struct {
		name     string
		decision extensionrun.StepDecision
		err      error
		want     bool
	}{
		{
			name:     "a confirmed step means sql-push started executing",
			decision: extensionrun.StepDecision{Confirm: true},
			want:     true,
		},
		{
			// The gate and --plan both reject on purpose. The extension ends having
			// run nothing, which is the point of rejecting.
			name:     "a rejected step sends nothing",
			decision: extensionrun.StepDecision{Confirm: false, Reason: "dry run (--plan)"},
			want:     false,
		},
		{
			// A decider that errors never answered the step at all.
			name:     "a decider that fails sends nothing",
			decision: extensionrun.StepDecision{Confirm: true},
			err:      errors.New("boom"),
			want:     false,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var p sqlPushProgress
			decide := p.track(func(extensionrun.StepPrompt) (extensionrun.StepDecision, error) {
				return tc.decision, tc.err
			})
			if _, err := decide(extensionrun.StepPrompt{}); (err != nil) != (tc.err != nil) {
				t.Fatalf("decider error = %v, want %v", err, tc.err)
			}
			if p.SQLIssued != tc.want {
				t.Errorf("SQLIssued = %v, want %v", p.SQLIssued, tc.want)
			}
		})
	}

	// A run that never reaches the confirmation step — the failure this whole
	// distinction exists for — leaves it false, because the decider is never called.
	var untouched sqlPushProgress
	if untouched.SQLIssued {
		t.Error("a run that never reached the confirmation step must not report SQL as issued")
	}

	// A caller that does not care passes nil, and gets its decider back unchanged.
	var nilProgress *sqlPushProgress
	if nilProgress.track(nil) != nil {
		t.Error("track(nil) on a nil progress should hand the decider straight back")
	}
}
