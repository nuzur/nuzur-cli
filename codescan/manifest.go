package codescan

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
)

const manifestName = ".nuzur-codegen-manifest.json"

// codegenManifest mirrors the .nuzur-codegen-manifest.json that go-code-gen
// writes next to every generated tree. The project_* fields are the stamp that
// lets a scan tie generated code back to its nuzur project; manifests written
// before the stamp existed simply leave them empty.
type codegenManifest struct {
	Version            int      `json:"version"`
	Generator          string   `json:"generator"`
	GeneratedAt        string   `json:"generated_at"`
	ProjectUUID        string   `json:"project_uuid"`
	ProjectVersionUUID string   `json:"project_version_uuid"`
	ProjectIdentifier  string   `json:"project_identifier"`
	Files              []string `json:"files"`
}

// generatedTree is one manifest found inside the scanned module.
type generatedTree struct {
	dir      string // absolute dir holding the manifest
	nodeID   string
	name     string
	manifest codegenManifest
	files    map[string]bool // absolute paths
}

// findManifests walks the module dir for generated trees. Hidden dirs,
// node_modules and vendor are skipped: go tooling never builds them either.
func findManifests(moduleDir, repoRoot string) ([]*generatedTree, error) {
	var trees []*generatedTree
	err := filepath.WalkDir(moduleDir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() {
			name := d.Name()
			if path != moduleDir && (strings.HasPrefix(name, ".") || strings.HasPrefix(name, "_") || name == "node_modules" || name == "vendor" || name == "testdata") {
				return filepath.SkipDir
			}
			return nil
		}
		if d.Name() != manifestName {
			return nil
		}
		m, err := readManifest(path)
		if err != nil {
			return nil
		}
		dir := filepath.Dir(path)
		t := &generatedTree{dir: dir, manifest: m, files: map[string]bool{}}
		for _, f := range m.Files {
			t.files[filepath.Join(dir, filepath.FromSlash(f))] = true
		}
		rel, _ := filepath.Rel(repoRoot, dir)
		rel = filepath.ToSlash(rel)
		t.nodeID = "gen:" + rel
		t.name = m.ProjectIdentifier
		if t.name == "" {
			t.name = filepath.Base(dir)
		}
		trees = append(trees, t)
		return nil
	})
	sort.Slice(trees, func(i, j int) bool { return trees[i].dir < trees[j].dir })
	return trees, err
}

func readManifest(path string) (codegenManifest, error) {
	var m codegenManifest
	raw, err := os.ReadFile(path)
	if err != nil {
		return m, err
	}
	err = json.Unmarshal(raw, &m)
	return m, err
}

// depModule is a dependency module that carries a codegen manifest at its root
// (e.g. nuzur-go depending on the generated nem module).
type depModule struct {
	path     string
	nodeID   string
	name     string
	manifest codegenManifest
}

// findGeneratedDeps lists the module graph and keeps the dependencies whose
// module root holds a codegen manifest. Failures are non-fatal: a scan without
// dependency detection is still a correct scan of the module itself.
func findGeneratedDeps(ctx context.Context, moduleDir string) []*depModule {
	cmd := exec.CommandContext(ctx, "go", "list", "-m", "-json", "all")
	cmd.Dir = moduleDir
	out, err := cmd.Output()
	if err != nil {
		return nil
	}
	var deps []*depModule
	dec := json.NewDecoder(bufio.NewReader(bytes.NewReader(out)))
	for dec.More() {
		var mod struct {
			Path string
			Dir  string
			Main bool
		}
		if err := dec.Decode(&mod); err != nil {
			break
		}
		if mod.Main || mod.Dir == "" {
			continue
		}
		m, err := readManifest(filepath.Join(mod.Dir, manifestName))
		if err != nil {
			continue
		}
		name := m.ProjectIdentifier
		if name == "" {
			name = filepath.Base(mod.Path)
		}
		deps = append(deps, &depModule{path: mod.Path, nodeID: "gen:" + mod.Path, name: name, manifest: m})
	}
	sort.Slice(deps, func(i, j int) bool { return deps[i].path < deps[j].path })
	return deps
}
