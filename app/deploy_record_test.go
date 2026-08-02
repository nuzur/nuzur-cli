package app

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/nuzur/nuzur-cli/deploy"
)

// deploy_record_test.go covers the record STORE as the deploy pipeline uses it:
// that every production write goes through the mutator, that the checkpoints are
// written in the order the deploy happens in, and that a merge write really does
// preserve what an earlier deploy of the same box recorded.
//
// The transcript goldens cover what the user is told; this file covers what
// `destroy`, `deploy list` and the next deploy will read. The two together are
// the wave's acceptance gate, because the bugs it retires are precisely the ones
// where the terminal was right and the record was not.

// ── the one write path ───────────────────────────────────────────────────────

// deploy.SaveDeployment replaces a record wholesale, which is the shape of bug
// this wave removed: a caller assembling a Deployment literal has to remember
// every field, and three separate bugs came from one of them forgetting.
// Production code writes through deploy.MutateDeployment; SaveDeployment stays
// exported only so tests can seed a machine's record store with records that
// already exist in full.
//
// Checked over the AST rather than by grepping so that the word appearing in a
// comment — it appears in several, explaining exactly this — is not a failure.
func TestAppWritesRecordsOnlyThroughMutateDeployment(t *testing.T) {
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("cannot locate the package directory")
	}
	dir := filepath.Dir(thisFile)

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("reading the package directory: %v", err)
	}
	fset := token.NewFileSet()
	scanned := 0
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		scanned++
		file, err := parser.ParseFile(fset, filepath.Join(dir, name), nil, 0)
		if err != nil {
			t.Fatalf("parsing %s: %v", name, err)
		}
		ast.Inspect(file, func(n ast.Node) bool {
			sel, ok := n.(*ast.SelectorExpr)
			if !ok || sel.Sel.Name != "SaveDeployment" {
				return true
			}
			pkg, ok := sel.X.(*ast.Ident)
			if !ok || pkg.Name != "deploy" {
				return true
			}
			t.Errorf("%s:%d calls deploy.SaveDeployment — production record writes must go through "+
				"deploy.MutateDeployment, or the write silently drops every field it did not restate",
				name, fset.Position(sel.Pos()).Line)
			return true
		})
	}
	if scanned == 0 {
		t.Fatal("scanned no files; the package directory lookup is wrong and this test proves nothing")
	}
}

// ── checkpoints ──────────────────────────────────────────────────────────────

// soleRecord reads back the single deployment record on the isolated machine.
// Used from inside the fakes' observation hooks, so it takes the failure rather
// than reporting it: a t.Fatalf from a non-test goroutine would be undefined, and
// these hooks run on the deploy's own goroutine where a return is safer.
func soleRecord(t *testing.T) (deploy.Deployment, bool) {
	t.Helper()
	deps, err := deploy.ListDeployments()
	if err != nil || len(deps) != 1 {
		return deploy.Deployment{}, false
	}
	return deps[0], true
}

// The checkpoint SEQUENCE of a managed first deploy, observed while it runs.
//
// The final state is asserted by the golden tests; what this adds is order. A
// checkpoint written in the wrong place is invisible at the end — the record
// still reads `finalized` — and yet it is the whole value of the field: wave 5
// reads it to decide whether the last run got far enough to be reused, and a
// checkpoint that ran ahead of the fact it describes would answer that question
// with a claim rather than with evidence.
//
// The three mid-run windows are the fakes' observation hooks: the top of the
// provider create call, the instant the VM is acknowledged, and the bootstrap.
// They are read from the real record files under the isolated HOME.
func TestDeployRecordSequenceManagedFirstDeploy(t *testing.T) {
	type observation struct{ where, step string }
	var seen []observation
	observe := func(where string) {
		rec, ok := soleRecord(t)
		if !ok {
			seen = append(seen, observation{where, "<no single record>"})
			return
		}
		seen = append(seen, observation{where, rec.LastCompletedStep})
	}

	g := runDeployGolden(t, "first_managed_deploy", goldenEnv{
		args:    managedDeployArgs(),
		product: firstDeployAgents,
		er: func(f *fakeExtensionRunner) {
			f.setLastGoCodeGenConfig(savedGoCodeGenConfig())
		},
		prov: func(p *fakeProvisioner) {
			p.BeforeProvision = func() { observe("provider create call") }
			p.AfterInstanceCreated = func() { observe("instance acknowledged") }
		},
		ssh: func(r *fakeRemoteRunner) {
			r.BeforeRunScript = func(string) {
				// Only the first script: the bootstrap is one call today, and a
				// second one would otherwise silently change what is asserted.
				if len(r.Scripts()) == 0 {
					observe("bootstrap")
				}
			}
			r.BeforeCapture = func(command string) {
				// The front-door readback, which happens after the agent paired
				// and before the record is finalized. (The ports readback runs
				// later still, from the revision report.)
				if strings.Contains(command, "/url") {
					observe("front-door readback")
				}
			}
		},
	})
	if g.exit != 0 {
		t.Fatalf("exit = %d, want 0", g.exit)
	}
	rec := g.onlyDeployment(t)
	seen = append(seen, observation{"after the run", rec.LastCompletedStep})

	want := []observation{
		{"provider create call", deploy.StepPendingRecorded},
		{"instance acknowledged", deploy.StepInstanceCreated},
		{"bootstrap", deploy.StepBoxRecorded},
		{"front-door readback", deploy.StepAgentPaired},
		{"after the run", deploy.StepFinalized},
	}
	if len(seen) != len(want) {
		t.Fatalf("observed %d checkpoints, want %d: %+v", len(seen), len(want), seen)
	}
	for i := range want {
		if seen[i] != want[i] {
			t.Errorf("at %q the record read %q, want %q (full sequence: %+v)",
				seen[i].where, seen[i].step, want[i].step, seen)
		}
	}
	// The same claim in the form wave 5 will read it: never backwards.
	for i := 1; i < len(seen); i++ {
		if deploy.StepRank(seen[i].step) <= deploy.StepRank(seen[i-1].step) {
			t.Errorf("checkpoint went backwards or stalled between %q and %q: %+v",
				seen[i-1].where, seen[i].where, seen)
		}
	}
	if rec.LocalAgentUUID != fakeAgentUUID {
		t.Errorf("LocalAgentUUID = %q, want the paired agent", rec.LocalAgentUUID)
	}
}

// A re-deploy no longer blanks the front door for the middle of its run.
//
// The record is written as soon as the box exists (6b) and finalized at step 12,
// and between those two points is every slow step of the deploy — copy, build,
// bootstrap, pairing. The 6b write used to REPLACE the record with a freshly
// assembled struct that had no URL fields on it, so for those twenty minutes the
// recorded deployment of a perfectly healthy box had no API url, no public url
// and no data-manager link; a run interrupted in that window left them gone for
// good. With a merge write they survive, and this pins it at the one moment it
// is observable.
func TestRedeployPreservesURLsMidRun(t *testing.T) {
	var midRun deploy.Deployment
	var midRunSeen bool

	g := runDeployGolden(t, "redeploy_reuse_clean", goldenEnv{
		args: managedDeployArgs(),
		seed: func(work string) []*deploy.Deployment {
			return []*deploy.Deployment{deployedRecord(work)}
		},
		er: func(f *fakeExtensionRunner) {
			f.setLastGoCodeGenConfig(savedGoCodeGenConfig())
		},
		ssh: func(r *fakeRemoteRunner) {
			r.BeforeRunScript = func(string) {
				if len(r.Scripts()) > 0 {
					return
				}
				midRun, midRunSeen = soleRecord(t)
			}
		},
	})
	if g.exit != 0 {
		t.Fatalf("exit = %d, want 0", g.exit)
	}
	if !midRunSeen {
		t.Fatal("the mid-run observation never happened; the test asserts nothing")
	}
	// The window really is between the two writes: the box has been recorded and
	// the deploy has not finished.
	//
	// It is also where the checkpoint's one non-monotone transition is
	// observable: the record was seeded `finalized` and reads `box_recorded`
	// here, so a healthy serving box is described as half-deployed for the middle
	// of every re-deploy. Deliberate — Deployment.LastCompletedStep says why
	// blanking it instead would be worse — and asserted here so a change to it
	// has to come through this line and argue the case.
	if midRun.LastCompletedStep != deploy.StepBoxRecorded {
		t.Fatalf("mid-run checkpoint = %q, want %q — the observation is not in the window it claims to be",
			midRun.LastCompletedStep, deploy.StepBoxRecorded)
	}
	seeded := deployedRecord(g.work)
	if midRun.APIURL != seeded.APIURL || midRun.PublicURL != seeded.PublicURL {
		t.Errorf("the front door was blanked for the middle of the re-deploy: api=%q public=%q, want %q/%q",
			midRun.APIURL, midRun.PublicURL, seeded.APIURL, seeded.PublicURL)
	}
	// The other things a re-deploy knows before it finishes, on the same write.
	if midRun.LocalAgentUUID != fakeAgentUUID {
		t.Errorf("mid-run LocalAgentUUID = %q, want the known agent %q", midRun.LocalAgentUUID, fakeAgentUUID)
	}
	if midRun.ProviderResourceName != seedResourceName || midRun.ProviderInstanceID != fakeInstanceID {
		t.Errorf("mid-run provider handles lost: name=%q id=%q", midRun.ProviderResourceName, midRun.ProviderInstanceID)
	}
	if midRun.Provisioning {
		t.Error("mid-run record is marked provisioning although the box already existed")
	}
	// And the finished record is complete, so the preservation is not a stale
	// value being carried past the point where the real one is known.
	rec := g.onlyDeployment(t)
	if rec.LastCompletedStep != deploy.StepFinalized || rec.APIURL == "" || rec.DataManagerURL == "" {
		t.Errorf("record not finalized: %+v", rec)
	}
}
