package extensionrun

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"golang.org/x/mod/modfile"
)

// The workspace's go.mod after an extraction, in one place.
//
// Generation runs REMOTELY, in a fresh tree that contains only what the generator
// itself emits — the developer's custom `app/` zone exists nowhere on that
// machine. The generator's `go mod init` + `go mod tidy` therefore answer a
// narrower question than the workspace asks: "what do the GENERATED files
// import", not "what does this workspace import". Its go.mod is authoritative for
// the former and incomplete for the latter, and extraction overwrites the local
// copy with it (see generatorManagedFiles).
//
// What that costs, and what is done about it here:
//
//   - requires needed only by user-owned code (the reported case: `app/` imports
//     golang.org/x/time/rate, the archive's go.mod does not require it, and
//     `go build ./...` fails with "no required module provides package"). Fixed by
//     running `go mod tidy` in the workspace AFTER extraction, where both the
//     generated code and the preserved user files are on disk.
//
//   - directives tidy cannot re-derive because no import implies them: `replace`
//     (a developer pointing a dependency at a local checkout to debug it),
//     `exclude`, `tool`, `godebug`. Tidy re-derives requires from imports and
//     nothing else, so these would be silently destroyed by the overwrite and
//     never come back. They are snapshotted before extraction and re-applied
//     before the tidy.
//
// Both are BEST-EFFORT and never fatal: they need a Go toolchain and usually the
// module proxy, and neither is required for the deploy to succeed — the image
// build runs its own `go mod tidy` inside Docker. A developer with no `go` on
// PATH, or behind a broken proxy, still gets a completed deploy plus a warning
// telling them what to run.

// ErrGoToolchainMissing reports that `go` could not be started at all. Mirrors
// go-code-gen's classification of the same failure: it means nothing is wrong
// with the files, only that they have not been reconciled.
var ErrGoToolchainMissing = errors.New("no go toolchain on PATH")

// goModTidyTimeout bounds the tidy so a hung module proxy cannot hang a deploy.
// Generous, because a first tidy on a cold module cache downloads the whole
// dependency graph.
const goModTidyTimeout = 5 * time.Minute

// goModuleRoots returns the directories at or just below outputPath that hold a
// go.mod.
//
// It mirrors files.FindGeneratedManifest's root contract, and for the same
// reason: the extraction root and the generated project's root are not the same
// directory. The generator nests its whole output under one folder named after
// the identifier, so extracting into `nuzur-chorus/` puts go.mod at
// `nuzur-chorus/chorus/go.mod`. Only outputPath and its immediate
// subdirectories are considered — that is the generator's nesting contract, and
// a deeper walk would find the go.mod of an unrelated project a user happens to
// keep inside the workspace.
//
// All candidates are returned rather than one, so the caller can tell "not a Go
// project" (none) from "ambiguous layout" (several, e.g. after the generated
// root was renamed and the old tree is still there) and say so.
func goModuleRoots(outputPath string) []string {
	if regularFileExists(filepath.Join(outputPath, "go.mod")) {
		return []string{outputPath}
	}
	entries, err := os.ReadDir(outputPath)
	if err != nil {
		return nil
	}
	var roots []string
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		dir := filepath.Join(outputPath, e.Name())
		if regularFileExists(filepath.Join(dir, "go.mod")) {
			roots = append(roots, dir)
		}
	}
	return roots
}

// goModuleRoot returns the single module root under outputPath, if there is
// exactly one.
func goModuleRoot(outputPath string) (string, bool) {
	roots := goModuleRoots(outputPath)
	if len(roots) != 1 {
		return "", false
	}
	return roots[0], true
}

// localModuleEdits are the parts of a workspace go.mod that the generator never
// emits AND `go mod tidy` cannot re-derive from the code, so they exist only in
// the local copy the archive is about to overwrite.
//
// Deliberately not requires: those ARE re-derived by the tidy, from the imports
// in the user-owned files extraction preserves, so carrying them over would only
// resurrect requires the user has since stopped importing.
//
// `retract` and `ignore` are left out on purpose: both are module-PUBLISHER
// directives, and a generated application is not published as a library.
type localModuleEdits struct {
	Replace []*modfile.Replace
	Exclude []*modfile.Exclude
	Tool    []*modfile.Tool
	Godebug []*modfile.Godebug
}

func (e localModuleEdits) empty() bool {
	return len(e.Replace) == 0 && len(e.Exclude) == 0 && len(e.Tool) == 0 && len(e.Godebug) == 0
}

// readLocalModuleEdits reads the preservable directives from a go.mod. A missing
// file is not an error (first generation into an empty workspace).
func readLocalModuleEdits(goModPath string) (localModuleEdits, error) {
	data, err := os.ReadFile(goModPath) // #nosec G304 - path is the workspace go.mod
	if err != nil {
		if os.IsNotExist(err) {
			return localModuleEdits{}, nil
		}
		return localModuleEdits{}, err
	}
	f, err := modfile.Parse(goModPath, data, nil)
	if err != nil {
		return localModuleEdits{}, fmt.Errorf("parsing %s: %w", goModPath, err)
	}
	return localModuleEdits{
		Replace: f.Replace,
		Exclude: f.Exclude,
		Tool:    f.Tool,
		Godebug: f.Godebug,
	}, nil
}

// restoreLocalModuleEdits re-applies edits to the go.mod now on disk, skipping
// any the incoming copy already carries, and returns a description of each one
// restored. Nothing is written when there is nothing to restore.
func restoreLocalModuleEdits(goModPath string, edits localModuleEdits) ([]string, error) {
	if edits.empty() {
		return nil, nil
	}
	data, err := os.ReadFile(goModPath) // #nosec G304 - path is the workspace go.mod
	if err != nil {
		return nil, err
	}
	f, err := modfile.Parse(goModPath, data, nil)
	if err != nil {
		return nil, fmt.Errorf("parsing %s: %w", goModPath, err)
	}

	var restored []string
	for _, r := range edits.Replace {
		if hasReplace(f, r) {
			continue
		}
		if err := f.AddReplace(r.Old.Path, r.Old.Version, r.New.Path, r.New.Version); err != nil {
			return restored, fmt.Errorf("restoring replace %s: %w", replaceString(r), err)
		}
		restored = append(restored, "replace "+replaceString(r))
	}
	for _, x := range edits.Exclude {
		if hasExclude(f, x) {
			continue
		}
		if err := f.AddExclude(x.Mod.Path, x.Mod.Version); err != nil {
			return restored, fmt.Errorf("restoring exclude %s %s: %w", x.Mod.Path, x.Mod.Version, err)
		}
		restored = append(restored, fmt.Sprintf("exclude %s %s", x.Mod.Path, x.Mod.Version))
	}
	for _, t := range edits.Tool {
		if hasTool(f, t.Path) {
			continue
		}
		if err := f.AddTool(t.Path); err != nil {
			return restored, fmt.Errorf("restoring tool %s: %w", t.Path, err)
		}
		restored = append(restored, "tool "+t.Path)
	}
	for _, g := range edits.Godebug {
		if hasGodebug(f, g.Key) {
			continue
		}
		if err := f.AddGodebug(g.Key, g.Value); err != nil {
			return restored, fmt.Errorf("restoring godebug %s: %w", g.Key, err)
		}
		restored = append(restored, fmt.Sprintf("godebug %s=%s", g.Key, g.Value))
	}
	if len(restored) == 0 {
		return nil, nil
	}

	f.Cleanup()
	out, err := f.Format()
	if err != nil {
		return nil, fmt.Errorf("formatting %s: %w", goModPath, err)
	}
	mode := os.FileMode(0o600)
	if info, err := os.Stat(goModPath); err == nil {
		mode = info.Mode().Perm()
	}
	if err := os.WriteFile(goModPath, out, mode); err != nil { // #nosec G306 - keeps the file's existing mode
		return nil, err
	}
	return restored, nil
}

func hasReplace(f *modfile.File, r *modfile.Replace) bool {
	for _, existing := range f.Replace {
		if existing.Old.Path == r.Old.Path && existing.Old.Version == r.Old.Version {
			return true
		}
	}
	return false
}

func hasExclude(f *modfile.File, x *modfile.Exclude) bool {
	for _, existing := range f.Exclude {
		if existing.Mod.Path == x.Mod.Path && existing.Mod.Version == x.Mod.Version {
			return true
		}
	}
	return false
}

func hasTool(f *modfile.File, path string) bool {
	for _, existing := range f.Tool {
		if existing.Path == path {
			return true
		}
	}
	return false
}

func hasGodebug(f *modfile.File, key string) bool {
	for _, existing := range f.Godebug {
		if existing.Key == key {
			return true
		}
	}
	return false
}

func replaceString(r *modfile.Replace) string {
	old := r.Old.Path
	if r.Old.Version != "" {
		old += " " + r.Old.Version
	}
	newer := r.New.Path
	if r.New.Version != "" {
		newer += " " + r.New.Version
	}
	return old + " => " + newer
}

// goModTidy runs `go mod tidy` in dir, folding the command's own output into the
// error — that output is where the real reason lives (a proxy failure, an
// unresolvable import), and the caller reports it verbatim.
func goModTidy(dir string) error {
	ctx, cancel := context.WithTimeout(context.Background(), goModTidyTimeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, "go", "mod", "tidy")
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err == nil {
		return nil
	}
	var execErr *exec.Error
	if errors.As(err, &execErr) && errors.Is(execErr.Err, exec.ErrNotFound) {
		return fmt.Errorf("%w: cannot run go mod tidy in %s", ErrGoToolchainMissing, dir)
	}
	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		return fmt.Errorf("go mod tidy timed out after %s in %s (module proxy unreachable?)", goModTidyTimeout, dir)
	}
	return fmt.Errorf("go mod tidy failed in %s: %w\noutput: %s", dir, err, strings.TrimSpace(string(out)))
}

// snapshotLocalModuleEdits reads the preservable directives out of the workspace
// go.mod before extraction overwrites it. Best-effort: an unreadable or
// unparsable local go.mod means there is nothing to carry over, which is exactly
// what the pre-fix behavior did, so it warns and continues.
func snapshotLocalModuleEdits(outputPath string) localModuleEdits {
	root, ok := goModuleRoot(outputPath)
	if !ok {
		return localModuleEdits{}
	}
	edits, err := readLocalModuleEdits(filepath.Join(root, "go.mod"))
	if err != nil {
		fmt.Fprintf(os.Stderr, "Warning: could not read the workspace go.mod, so any replace/exclude directives in it will not be carried over: %v\n", err)
		return localModuleEdits{}
	}
	return edits
}

// reconcileWorkspaceModule restores the local directives the archive's go.mod
// dropped and re-runs `go mod tidy` over the workspace as it now stands —
// generated code plus the user-owned files extraction preserved.
//
// Never returns an error: a workspace whose go.mod could not be reconciled is a
// workspace that may not BUILD LOCALLY, which is worth a loud warning and not
// worth failing a deploy that has already succeeded everywhere else.
func reconcileWorkspaceModule(outputPath string, edits localModuleEdits) {
	roots := goModuleRoots(outputPath)
	switch len(roots) {
	case 0:
		return // not a Go project (or no go.mod at the generator's nesting depth)
	case 1:
	default:
		fmt.Fprintf(os.Stderr,
			"Warning: found more than one go.mod under %s (%s); skipping `go mod tidy`. Run it yourself in the generated project's directory.\n",
			outputPath, strings.Join(roots, ", "))
		return
	}
	root := roots[0]

	if restored, err := restoreLocalModuleEdits(filepath.Join(root, "go.mod"), edits); err != nil {
		fmt.Fprintf(os.Stderr,
			"Warning: could not restore directives from your previous go.mod: %v\nCheck %s for missing replace/exclude directives.\n",
			err, filepath.Join(root, "go.mod"))
	} else if len(restored) > 0 {
		fmt.Fprintf(os.Stderr, "Restored %d directive(s) from your go.mod that the generated one does not carry:\n", len(restored))
		for _, r := range restored {
			fmt.Fprintf(os.Stderr, "  - %s\n", r)
		}
	}

	fmt.Fprintln(os.Stderr, "Reconciling go.mod with the workspace (go mod tidy)...")
	if err := goModTidy(root); err != nil {
		fmt.Fprintf(os.Stderr,
			"Warning: go mod tidy did not run in %s: %v\n"+
				"The generated go.mod covers the generated code only, so requires needed by your own code (app/) may be missing and `go build ./...` can fail locally. The deploy itself is unaffected — the container build tidies again. Run `go mod tidy` in %s once you have a Go toolchain and network access.\n",
			root, err, root)
	}
}
