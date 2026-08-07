package app

import (
	"context"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"

	"github.com/nuzur/nuzur-cli/deploy"
)

// deploy_pipeline_test.go asserts things about the SHAPE of the deploy, not
// about any run of it.
//
// The bugs this wave came out of were not step bugs. Six rounds of live
// validation produced nineteen findings, and the four that recurred were
// properties of the ORDER: a check placed after the thing it was meant to
// prevent, an effect with nothing on disk pointing at it, a record claiming a
// step that had not happened. None of those is visible in a step; all of them
// are visible in the list.
//
// So the list is data, and these tests read it. They need no HOME, no network
// and no fakes: deploySteps() allocates a description of the deploy and nothing
// else. A new step that gets any of this wrong fails here, in milliseconds, with
// the reason written out — which is the difference between a property that holds
// and a property somebody remembered.

// ── the [O] class: effect-before-check ───────────────────────────────────────

// stepsThatLegitimatelyRunLateWithoutEffects: steps that declare no effects and
// yet sit after the first provider-level one. Each is here because it CANNOT run
// earlier — not because it was convenient to leave it there. That is the whole
// content of the exemption, so it is stated per step.
var stepsThatLegitimatelyRunLateWithoutEffects = map[string]string{
	"ssh ping": "there is nothing to ping until the box exists; the equivalent check " +
		"for a box that already existed runs in `decide box`, before anything is spent",
	"read back front door": "reads what the bootstrap wrote, so it cannot precede it",
	"report":               "terminal output and the exit code; it is the last step by definition",

	// The k8s steps. `provision` is what this test measures lateness from, and
	// for ProviderK8s it is a no-op: K8sProvisioner creates nothing and bills
	// nothing, because the user already owns the cluster. Each of these still
	// has a real reason it cannot move above it.
	"resolve cluster": "needs the SSH runner, which does not exist until `ssh ping` — the same " +
		"constraint that exempts `ssh ping` itself. It is deliberately the FIRST k8s step so " +
		"an unreachable cluster is refused before anything is generated, committed or built",
	"wait for ci": "waits on the build for a specific commit, which does not exist until " +
		"`commit and push` has made one",
	"resolve image": "names the image built for that commit, so it cannot precede the commit",
	"read back cluster address": "reads the Service or Ingress the release created, so it " +
		"cannot precede it (the k8s counterpart of `read back front door`)",
}

// A pure step after the money has started is a check in the wrong place.
//
// This is the [O] class ("effect before check") as an assertion. Four of the
// nineteen findings were one shape: something was created, billed for, or shipped
// and only then was the question asked that would have stopped it — the schema
// that could not render, the CLI release that did not exist, the reused box that
// did not answer. Each fix moved a check earlier. This test is what keeps them
// there, and what makes the next one hard to add in the wrong place: a new
// no-effect step after `provision` must either move up or explain itself above.
func TestDeployStepsChecksRunBeforeEffects(t *testing.T) {
	steps := deploySteps()
	firstProvider := -1
	for idx, s := range steps {
		if s.effects.Has(effProvider) {
			firstProvider = idx
			break
		}
	}
	if firstProvider < 0 {
		t.Fatal("no step declares a provider effect — a deploy that creates no billed resource " +
			"means this test is asserting nothing; fix the declarations, not the test")
	}
	for _, s := range steps[firstProvider+1:] {
		if s.effects.Max() != effNone {
			continue
		}
		if why, ok := stepsThatLegitimatelyRunLateWithoutEffects[s.name]; ok {
			if strings.TrimSpace(why) == "" {
				t.Errorf("step %q is exempted with an empty reason", s.name)
			}
			continue
		}
		t.Errorf("step %q leaves nothing behind but runs after %q, which bills for a server.\n"+
			"A step that only reads or decides belongs BEFORE the first provider effect, where "+
			"refusing costs nothing. If it genuinely cannot run earlier, say why in "+
			"stepsThatLegitimatelyRunLateWithoutEffects.",
			s.name, steps[firstProvider].name)
	}
}

// ── the [S]/[R] classes: an effect nothing on disk remembers ─────────────────

// stepsDeliberatelyWithoutCheckpoint: steps that change something outside this
// process and still write no checkpoint. Each entry is a decision, and the
// reason is the decision's content: what an interrupt at that step leaves
// behind, and why the next run does not need the record to know about it.
//
// Read together, this map is the honest version of "what is not recovered".
var stepsDeliberatelyWithoutCheckpoint = map[string]string{
	"issue provisioning token": "the token is single-use and short-lived; an unused one expires " +
		"and there is nothing to clean up",
	"report in progress": "best-effort progress reporting — the revision it opens is superseded " +
		"by the next deploy's, and the LOCAL record is what destroy reads",
	"provider firewall": "best-effort: the box's own ufw is the authoritative gate, and the rules " +
		"are re-applied by the next deploy of the same box",
	"copy source": "scratch under /tmp on the box, re-copied by every run that needs it",
	"write host config": "writes /etc/config/<identifier>/prod.yaml via a temp file and mv, so an " +
		"interrupt leaves either no file or a complete one — never a half-written config the app " +
		"would read and fail on obscurely. The next run sees the file exists and leaves it alone, " +
		"which is also what it does for a file the operator wrote by hand: this step never " +
		"overwrites, so there is no prior state a checkpoint would protect",
	"bootstrap": "idempotent by design and re-run in full by the next deploy; the box's existence " +
		"is already recorded by `record box`, which is what destroy needs",
	"publish catalog": "best-effort, and re-published as the UNION on the next deploy of any " +
		"project on this box",
	"apply schema": "a real gap, deliberately: the record has no checkpoint between agent_paired " +
		"and finalized, and the schema outcome is reported on the REVISION instead. A re-deploy " +
		"recomputes the diff against the live database rather than trusting a record",
	"finalize revision": "the cloud half of a deploy the local record has already finalized",
}

// Anything with a consequence should leave a trace the next run can read.
//
// The [S] and [R] classes: state written wholesale, and interrupted runs with no
// way back. A deploy that creates a VM, pairs an agent or completes a record and
// records nothing about having done so is how one box ended up with two records
// and a droplet nothing on disk pointed at.
func TestDeployStepsEffectfulStepsCheckpoint(t *testing.T) {
	for _, s := range deploySteps() {
		if s.effects.Max() <= effLocalFS {
			continue // pure, or a workspace on this machine the user can see
		}
		if s.checkpoint != "" {
			continue
		}
		if why, ok := stepsDeliberatelyWithoutCheckpoint[s.name]; ok {
			if strings.TrimSpace(why) == "" {
				t.Errorf("step %q is exempted with an empty reason", s.name)
			}
			continue
		}
		t.Errorf("step %q has effects (%s) and writes no checkpoint.\n"+
			"An interrupt here would leave something behind that the next run cannot see it did. "+
			"Give it a deploy.Step* checkpoint, or record in stepsDeliberatelyWithoutCheckpoint "+
			"what an interrupt at this step leaves and why that is recoverable without one.",
			s.name, s.effects)
	}
	// The exemptions describe steps that exist.
	names := map[string]bool{}
	for _, s := range deploySteps() {
		names[s.name] = true
	}
	for name := range stepsDeliberatelyWithoutCheckpoint {
		if !names[name] {
			t.Errorf("stepsDeliberatelyWithoutCheckpoint mentions %q, which is not a step — "+
				"a stale exemption silently excuses whatever is named next", name)
		}
	}
	for name := range stepsThatLegitimatelyRunLateWithoutEffects {
		if !names[name] {
			t.Errorf("stepsThatLegitimatelyRunLateWithoutEffects mentions %q, which is not a step", name)
		}
	}
}

// ── checkpoints go forwards ──────────────────────────────────────────────────

// The checkpoints appear in the list in rank order.
//
// deploy.StepRank exists so wave 5 can ask "did the last run get far enough to
// reuse?", and that question is only meaningful if a later step never writes an
// earlier checkpoint. The record test (TestDeployRecordSequenceManagedFirstDeploy)
// observes this happening on one scenario; this proves it about the list, for
// every scenario, including the ones no golden covers.
func TestDeployStepsCheckpointOrderMatchesStepOrder(t *testing.T) {
	prevRank, prevStep := 0, "(the start of the deploy)"
	seen := map[string]bool{}
	for _, s := range deploySteps() {
		if s.checkpoint == "" {
			continue
		}
		rank := deploy.StepRank(s.checkpoint)
		if rank == 0 {
			t.Errorf("step %q declares checkpoint %q, which has no rank — StepRank would read it "+
				"as a pre-checkpoint record and every comparison against it would be wrong",
				s.name, s.checkpoint)
			continue
		}
		if seen[s.checkpoint] {
			t.Errorf("checkpoint %q is written by more than one step (%q); which run got how far "+
				"stops being answerable", s.checkpoint, s.name)
		}
		seen[s.checkpoint] = true
		if rank <= prevRank {
			t.Errorf("step %q writes %q (rank %d) after %q wrote rank %d — the checkpoint goes "+
				"backwards, so a record could report less progress than it has",
				s.name, s.checkpoint, rank, prevStep, prevRank)
		}
		prevRank, prevStep = rank, s.name
	}
	if prevRank == 0 {
		t.Fatal("no step declares a checkpoint; this test is asserting nothing")
	}
}

// ── the declaration is not a wish ────────────────────────────────────────────

// checkpointConstNames maps each checkpoint VALUE to the identifier the source
// writes it as, so the test below can look for it. Restated here on purpose: if
// deploy/state.go renames a constant or changes a value, this map stops matching
// and says so, rather than the test quietly finding nothing to check.
var checkpointConstNames = map[string]string{
	deploy.StepPendingRecorded: "StepPendingRecorded",
	deploy.StepInstanceCreated: "StepInstanceCreated",
	deploy.StepBoxRecorded:     "StepBoxRecorded",
	deploy.StepAgentPaired:     "StepAgentPaired",
	deploy.StepReleased:        "StepReleased",
	deploy.StepFinalized:       "StepFinalized",
}

// A step that DECLARES a checkpoint must actually write it.
//
// deployStep.checkpoint is documented as declarative: the loop does not write
// checkpoints, because a checkpoint is only true if it lands in the same record
// write as the fact it describes (see the field's comment). That design is only
// safe if the declaration and the write cannot drift apart — otherwise the list
// would assert an ordering that the code does not implement, which is worse than
// asserting nothing.
//
// So: parse the step's own function and require the constant to appear in it.
// Structural rather than behavioural, deliberately — the behavioural version of
// this claim is TestDeployRecordSequenceManagedFirstDeploy, which can only cover
// the scenarios someone wrote a fake for.
func TestDeployStepsWriteTheCheckpointsTheyDeclare(t *testing.T) {
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
	bodies := map[string]*ast.FuncDecl{} // method name -> its declaration
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, filepath.Join(dir, name), nil, 0)
		if err != nil {
			t.Fatalf("parsing %s: %v", name, err)
		}
		for _, d := range f.Decls {
			fn, ok := d.(*ast.FuncDecl)
			if ok && fn.Recv != nil && fn.Body != nil {
				bodies[fn.Name.Name] = fn
			}
		}
	}
	if len(bodies) == 0 {
		t.Fatal("parsed no methods; the package lookup is wrong and this test proves nothing")
	}

	for _, s := range deploySteps() {
		if s.checkpoint == "" {
			continue
		}
		constName, ok := checkpointConstNames[s.checkpoint]
		if !ok {
			t.Errorf("step %q declares checkpoint %q, which checkpointConstNames does not know — "+
				"add it, or this step's checkpoint goes unchecked", s.name, s.checkpoint)
			continue
		}
		short := stepFuncName(s.run)
		fn, ok := bodies[short]
		if !ok {
			t.Errorf("cannot find the source of step %q (looked for method %q)", s.name, short)
			continue
		}
		found := false
		ast.Inspect(fn.Body, func(n ast.Node) bool {
			sel, ok := n.(*ast.SelectorExpr)
			if !ok || sel.Sel.Name != constName {
				return true
			}
			if x, ok := sel.X.(*ast.Ident); ok && x.Name == "deploy" {
				found = true
			}
			return true
		})
		if !found {
			t.Errorf("step %q declares checkpoint deploy.%s but %s never writes it.\n"+
				"The checkpoint is written INSIDE the step, in the same record write as the fact "+
				"it describes; a declaration with no write makes the step list claim an ordering "+
				"the record does not have.", s.name, constName, short)
		}
	}
}

// stepFuncName is the method name behind a step's run field. The list holds
// method EXPRESSIONS ((*Implementation).stepProvision), so the runtime name of
// the function ends in the method being named.
func stepFuncName(run func(*Implementation, context.Context, *deployState) error) string {
	full := runtime.FuncForPC(reflect.ValueOf(run).Pointer()).Name()
	full = strings.TrimSuffix(full, "-fm")
	if idx := strings.LastIndex(full, "."); idx >= 0 {
		full = full[idx+1:]
	}
	return full
}

// ── the effect set itself ────────────────────────────────────────────────────

func TestEffectSet(t *testing.T) {
	empty := effects()
	if empty.Max() != effNone || empty.Has(effProvider) || empty.String() != "none" {
		t.Errorf("the empty set is not empty: max=%v string=%q", empty.Max(), empty.String())
	}
	s := effects(effBox, effCloud)
	if !s.Has(effBox) || !s.Has(effCloud) {
		t.Error("Has does not report the levels the set was built from")
	}
	if s.Has(effProvider) || s.Has(effRecord) {
		t.Error("Has reports a level the set was not built from")
	}
	if s.Max() != effBox {
		t.Errorf("Max = %v, want box — the tiers are ordered by how hard they are to take back", s.Max())
	}
	if got := s.String(); got != "cloud|box" {
		t.Errorf("String = %q, want %q (lowest tier first)", got, "cloud|box")
	}
	if effects(effLocalFS, effProvider).Max() != effProvider {
		t.Error("Max is not the highest tier in the set")
	}
}

// Every step is described: a name, and a run.
func TestDeployStepsAreWellFormed(t *testing.T) {
	seen := map[string]bool{}
	for idx, s := range deploySteps() {
		if strings.TrimSpace(s.name) == "" {
			t.Errorf("step %d has no name; every failure message about it would be anonymous", idx)
		}
		if seen[s.name] {
			t.Errorf("two steps are called %q — the allowlists above key on the name", s.name)
		}
		seen[s.name] = true
		if s.run == nil {
			t.Errorf("step %q has no run function", s.name)
		}
	}
}
