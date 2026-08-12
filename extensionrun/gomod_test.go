package extensionrun

import (
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestGoModuleRoots(t *testing.T) {
	t.Run("flat layout", func(t *testing.T) {
		dir := t.TempDir()
		writeFile(t, filepath.Join(dir, "go.mod"), "module a\n")
		if roots := goModuleRoots(dir); len(roots) != 1 || roots[0] != dir {
			t.Errorf("goModuleRoots = %v, want [%s]", roots, dir)
		}
	})
	t.Run("nested under the generated project dir", func(t *testing.T) {
		dir := t.TempDir()
		proj := filepath.Join(dir, "chorus")
		writeFile(t, filepath.Join(proj, "go.mod"), "module a\n")
		if roots := goModuleRoots(dir); len(roots) != 1 || roots[0] != proj {
			t.Errorf("goModuleRoots = %v, want [%s]", roots, proj)
		}
	})
	t.Run("no go.mod at all", func(t *testing.T) {
		dir := t.TempDir()
		writeFile(t, filepath.Join(dir, "chorus", "main.ts"), "// not go\n")
		if roots := goModuleRoots(dir); len(roots) != 0 {
			t.Errorf("goModuleRoots = %v, want none for a non-Go project", roots)
		}
	})
	t.Run("ambiguous after the generated root was renamed", func(t *testing.T) {
		dir := t.TempDir()
		writeFile(t, filepath.Join(dir, "chorus", "go.mod"), "module a\n")
		writeFile(t, filepath.Join(dir, "chorus2", "go.mod"), "module b\n")
		if roots := goModuleRoots(dir); len(roots) != 2 {
			t.Errorf("goModuleRoots = %v, want both candidates so the caller can skip", roots)
		}
		if _, ok := goModuleRoot(dir); ok {
			t.Error("goModuleRoot must not guess between two module roots")
		}
	})
}

// TestRestoreLocalModuleEdits covers the half of the fix `go mod tidy` cannot do:
// tidy re-derives requires from imports, but NOTHING in a workspace implies a
// replace, an exclude, a tool or a godebug — so overwriting go.mod with the
// generator's copy destroyed them permanently and silently. A developer pointing
// a dependency at a local checkout to debug it lost that pointer on every deploy.
func TestRestoreLocalModuleEdits(t *testing.T) {
	dir := t.TempDir()
	local := filepath.Join(dir, "local.mod")
	writeFile(t, local, `module example.com/app

go 1.22

require example.com/only-local v1.2.3

exclude example.com/bad v1.0.0

tool example.com/cmd/gen

godebug default=go1.21

replace example.com/dep => ../dep

replace example.com/other v1.0.0 => example.com/other v1.1.0
`)
	edits, err := readLocalModuleEdits(local)
	if err != nil {
		t.Fatal(err)
	}

	// The archive's go.mod: the generator's answer, carrying none of the above.
	target := filepath.Join(dir, "go.mod")
	writeFile(t, target, "module example.com/app\n\ngo 1.22\n\nrequire example.com/generated v0.1.0\n")

	restored, err := restoreLocalModuleEdits(target, edits)
	if err != nil {
		t.Fatal(err)
	}
	if len(restored) != 5 {
		t.Errorf("restored = %v, want the 2 replaces + exclude + tool + godebug", restored)
	}

	got := read(t, dir, "go.mod")
	for _, want := range []string{
		"replace example.com/dep => ../dep",
		"example.com/other v1.0.0 => example.com/other v1.1.0",
		"exclude example.com/bad v1.0.0",
		"tool example.com/cmd/gen",
		"godebug default=go1.21",
		"example.com/generated v0.1.0",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("restored go.mod is missing %q:\n%s", want, got)
		}
	}
	// Requires are NOT carried over: tidy owns those, and resurrecting one the
	// user has stopped importing would undo the tidy's whole job.
	if strings.Contains(got, "example.com/only-local") {
		t.Errorf("a require from the old go.mod was carried over; tidy must own requires:\n%s", got)
	}
}

// A directive the incoming go.mod already carries must not be duplicated, and a
// go.mod that needs no restoration must not be rewritten at all.
func TestRestoreLocalModuleEdits_NoDuplicatesAndNoPointlessWrite(t *testing.T) {
	dir := t.TempDir()
	local := filepath.Join(dir, "local.mod")
	writeFile(t, local, "module a\n\ngo 1.22\n\nreplace example.com/dep => ../dep\n")
	edits, err := readLocalModuleEdits(local)
	if err != nil {
		t.Fatal(err)
	}

	target := filepath.Join(dir, "go.mod")
	const already = "module a\n\ngo 1.22\n\nreplace example.com/dep => ../dep\n"
	writeFile(t, target, already)

	restored, err := restoreLocalModuleEdits(target, edits)
	if err != nil {
		t.Fatal(err)
	}
	if len(restored) != 0 {
		t.Errorf("restored = %v, want nothing when the directive is already there", restored)
	}
	if got := read(t, dir, "go.mod"); got != already {
		t.Errorf("go.mod was rewritten needlessly:\n%s", got)
	}

	// No local go.mod at all (first generation into an empty workspace).
	empty, err := readLocalModuleEdits(filepath.Join(dir, "does-not-exist.mod"))
	if err != nil {
		t.Fatalf("a missing go.mod must not be an error: %v", err)
	}
	if !empty.empty() {
		t.Error("a missing go.mod must yield no edits")
	}
}

// TestApplyGeneratedArchive_TidyRecoversUserRequire is the regression test for
// the reported bug: a workspace whose user-owned app/ code imports a package the
// GENERATED code never imports. Generation runs remotely in a tree that has no
// app/, so the archive's go.mod does not require it; before this fix that go.mod
// overwrote the local one and `go build ./...` failed with "no required module
// provides package", while the deploy reported SUCCESS (the image build tidies
// again inside Docker).
//
// Hermetic: the dependency is a module on disk reached through a `replace`, so
// the whole test runs with GOPROXY=off. That makes it a test of BOTH halves at
// once — the replace has to survive the overwrite for the require to resolve.
func TestApplyGeneratedArchive_TidyRecoversUserRequire(t *testing.T) {
	requireGoToolchain(t)
	offlineGoEnv(t)

	// A tiny module standing in for golang.org/x/time.
	depDir := filepath.Join(t.TempDir(), "dep")
	writeFile(t, filepath.Join(depDir, "go.mod"), "module example.com/dep\n\ngo 1.22\n")
	writeFile(t, filepath.Join(depDir, "rate", "rate.go"), "package rate\n\nfunc New() int { return 1 }\n")

	dir := t.TempDir() // the workspace / --source-dir
	const project = "chorus"
	projDir := filepath.Join(dir, project)

	// The workspace as the developer left it: their own code in the custom zone,
	// and the go.mod they hand-edited to resolve it.
	userCode := "package httpx\n\nimport \"example.com/dep/rate\"\n\nfunc Limiter() int { return rate.New() }\n"
	writeFile(t, filepath.Join(projDir, "app", "ingest", "httpx", "client.go"), userCode)
	writeFile(t, filepath.Join(projDir, "go.mod"),
		"module example.com/chorus\n\ngo 1.22\n\nrequire example.com/dep v0.0.0\n\nreplace example.com/dep => "+depDir+"\n")

	// What the remote generator ships: generated code, an empty custom stub, and a
	// go.mod tidied against generated code ONLY — no example.com/dep, no replace.
	z := makeNestedZip(t, project, map[string]string{
		"main.go":                    genMarker + "package main\n\nfunc main() {}\n",
		"go.mod":                     "module example.com/chorus\n\ngo 1.22\n",
		"app/ingest/httpx/client.go": "package httpx\n",
	}, []string{"main.go"})

	if _, _, err := applyGeneratedArchive(z, dir); err != nil {
		t.Fatal(err)
	}

	// The user's code survived extraction (that part already worked)...
	if got := read(t, dir, filepath.Join(project, "app", "ingest", "httpx", "client.go")); got != userCode {
		t.Fatalf("user custom code was clobbered: %q", got)
	}
	// ...and the module file now describes the workspace, not just the generated
	// half of it.
	gomod := read(t, dir, filepath.Join(project, "go.mod"))
	if !strings.Contains(gomod, "example.com/dep") {
		t.Errorf("go.mod lost the require needed by user-owned code:\n%s", gomod)
	}
	if !strings.Contains(gomod, "replace example.com/dep") {
		t.Errorf("go.mod lost the user's replace directive:\n%s", gomod)
	}

	// The symptom itself: the workspace has to build locally.
	build := exec.Command("go", "build", "./...")
	build.Dir = projDir
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("go build ./... failed in the workspace after extraction:\n%s", string(out))
	}
}

// TestApplyGeneratedArchive_TidyFailureIsNotFatal: `go mod tidy` needs a Go
// toolchain and usually a module proxy, and a developer who has neither must
// still get a completed deploy — the image is built in Docker regardless. The
// failure is a warning that names the command to run, not an error.
func TestApplyGeneratedArchive_TidyFailureIsNotFatal(t *testing.T) {
	// An empty PATH is the "no go toolchain installed" case, exactly.
	t.Setenv("PATH", t.TempDir())

	dir := t.TempDir()
	const project = "chorus"
	z := makeNestedZip(t, project, map[string]string{
		"main.go": genMarker + "package main\n\nfunc main() {}\n",
		"go.mod":  "module example.com/chorus\n\ngo 1.22\n",
	}, []string{"main.go"})

	stderr := captureStderr(t, func() {
		if _, _, err := applyGeneratedArchive(z, dir); err != nil {
			t.Fatalf("a failing go mod tidy must not fail the deploy: %v", err)
		}
	})

	if !strings.Contains(stderr, "go mod tidy") {
		t.Errorf("the warning must name the command the user has to run:\n%s", stderr)
	}
	if !strings.Contains(stderr, filepath.Join(dir, project)) {
		t.Errorf("the warning must name the directory to run it in:\n%s", stderr)
	}
	// And the extraction itself still happened.
	if !regularFileExists(filepath.Join(dir, project, "go.mod")) {
		t.Error("extraction must complete even when the module cannot be reconciled")
	}
}

func TestGoModTidy_MissingToolchainIsClassified(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "go.mod"), "module a\n\ngo 1.22\n")
	err := goModTidy(dir)
	if !errors.Is(err, ErrGoToolchainMissing) {
		t.Errorf("goModTidy err = %v, want ErrGoToolchainMissing", err)
	}
}

// A workspace with no go.mod (a non-Go generator) must not be touched, and must
// not print anything about Go modules.
func TestReconcileWorkspaceModule_SkipsNonGoWorkspace(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "web", "index.ts"), "export const x = 1\n")
	stderr := captureStderr(t, func() {
		reconcileWorkspaceModule(dir, localModuleEdits{})
	})
	if stderr != "" {
		t.Errorf("a non-Go workspace must produce no module output, got:\n%s", stderr)
	}
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func requireGoToolchain(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("go"); err != nil {
		t.Skip("no go toolchain on PATH")
	}
}

// offlineGoEnv pins the go commands this package shells out to so they resolve
// everything locally: no proxy, no sumdb, no toolchain download.
func offlineGoEnv(t *testing.T) {
	t.Helper()
	t.Setenv("GOPROXY", "off")
	t.Setenv("GOFLAGS", "-mod=mod")
	t.Setenv("GONOSUMDB", "*")
	t.Setenv("GONOSUMCHECK", "1")
	t.Setenv("GOSUMDB", "off")
	t.Setenv("GOTOOLCHAIN", "local")
}

// captureStderr collects what fn writes to os.Stderr. The extract pipeline warns
// through os.Stderr directly (as the rest of this package does), so the seam is
// the variable itself.
func captureStderr(t *testing.T, fn func()) string {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	orig := os.Stderr
	os.Stderr = w
	done := make(chan string, 1)
	go func() {
		b, _ := io.ReadAll(r)
		done <- string(b)
	}()
	defer func() {
		os.Stderr = orig
	}()
	fn()
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	return <-done
}
