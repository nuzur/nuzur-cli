package app

import (
	"errors"
	"io"
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

// The sentinel exists so the first-deploy pre-check can tell "this project has no
// tables" (fine) from "this project's schema is broken" (not fine). Introducing it
// must not have changed what a create-mode `--plan` prints, because that message
// is the entire answer that path gives — hence the fragment-wrapping, and hence
// this test, which pins both halves at once.
func TestNoStandaloneEntitiesIsASentinelAndKeepsItsMessage(t *testing.T) {
	// computeCreatePlan narrates on the way to a render failure; this test is
	// about the errors it returns, not about that line.
	swapOutputWriters(t, io.Discard, io.Discard)

	i := &Implementation{}
	er := newFakeExtensionRunner()
	er.StandaloneEntities = nil
	targets := &runTargets{
		er:             er,
		project:        er.Project,
		projectVersion: er.ProjectVersion,
	}

	_, err := i.computeCreatePlan(targets, "mysql")
	if err == nil {
		t.Fatal("a project version with no standalone entities rendered a plan")
	}
	if !errors.Is(err, errNoStandaloneEntities) {
		t.Errorf("computeCreatePlan error does not match the sentinel: %v", err)
	}
	const want = "project version v_21 has no standalone entities, so there is no schema to create"
	if err.Error() != want {
		t.Errorf("message = %q, want %q — create-mode --plan prints this verbatim", err.Error(), want)
	}

	// And a real render failure is NOT the sentinel, or the pre-check would warn
	// its way past a broken schema.
	er2 := newFakeExtensionRunner()
	er2.CreateSQLErr = errors.New("extension execution failed")
	targets2 := &runTargets{er: er2, project: er2.Project, projectVersion: er2.ProjectVersion}
	if _, err := i.computeCreatePlan(targets2, "mysql"); err == nil || errors.Is(err, errNoStandaloneEntities) {
		t.Errorf("a render failure was reported as the no-entities sentinel: %v", err)
	}
}
