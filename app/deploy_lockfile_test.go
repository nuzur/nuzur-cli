package app

import (
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// aburridesSettings is the shape that actually caused this file to exist: a k8s
// deploy against an external database, with all three hostnames.
func aburridesSettings() *deploySettings {
	return &deploySettings{
		Provider: "k8s", Host: "69.164.192.246", User: "root", Port: 22,
		Project: "3028c1ae", Version: "1a93992a", Identifier: "aburrides",
		DB: "mysql", Connection: "98b8d037",
		Domain: "api.aburrides.com", AuthDomain: "auth.aburrides.com", GRPCDomain: "grpc.aburrides.com",
		ImageRepo: "ghcr.io/mklfarha/aburrides/aburrides",
		Namespace: "aburrides", Release: "aburrides",
		API: "both", Auth: "jwt",
		// The three that must not travel.
		DBDSN:     "root:sup3rs3cret@tcp(db.internal:3306)/aburrides",
		SSHKey:    "/Users/someone/.ssh/id_ed25519",
		SourceDir: "/Users/someone/Dropbox/aburrides/code/nuzur-aburrides",
		WebURL:    "https://staging.nuzur.com",
	}
}

// The file is meant to be committed, so what it must never contain matters more
// than what it does.
func TestDeployLockOmitsSecretsAndMachineLocalPaths(t *testing.T) {
	body, err := deployLockBytes(aburridesSettings().toDeployConfig(), "aburrides-73aa7738")
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	got := string(body)

	for _, secret := range []string{
		"sup3rs3cret",               // a database password, in git
		"/Users/someone/.ssh",       // a path on one laptop, plus a username
		"/Users/someone/Dropbox",    // an absolute local path
		"https://staging.nuzur.com", // the operator's endpoint, not the app's
	} {
		if strings.Contains(got, secret) {
			t.Errorf("lockfile contains %q:\n%s", secret, got)
		}
	}

	// And it must still be a usable description of the deploy, or stripping has
	// gone too far.
	for _, needed := range []string{
		"69.164.192.246", "api.aburrides.com", "auth.aburrides.com", "grpc.aburrides.com",
		"ghcr.io/mklfarha/aburrides/aburrides", "aburrides-73aa7738", "1a93992a", "98b8d037",
	} {
		if !strings.Contains(got, needed) {
			t.Errorf("lockfile is missing %q — it could not replay this deploy:\n%s", needed, got)
		}
	}
}

// On the k8s path `git add` is scoped to the app dir and runs every deploy, so a
// byte that changes when the deploy did not means a commit every run — and a commit
// means a CI image build. The cost of a stray timestamp here is minutes, not noise.
func TestDeployLockBytesAreDeterministic(t *testing.T) {
	first, err := deployLockBytes(aburridesSettings().toDeployConfig(), "aburrides-73aa7738")
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	second, err := deployLockBytes(aburridesSettings().toDeployConfig(), "aburrides-73aa7738")
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	if string(first) != string(second) {
		t.Error("two renders of the same deploy differ; every deploy would commit and trigger CI")
	}
	if when := regexp.MustCompile(`\d{4}-\d{2}-\d{2}T\d{2}:\d{2}`).FindString(string(first)); when != "" {
		t.Errorf("lockfile carries a timestamp (%q); it must only change when the deploy does", when)
	}
}

// The file has to BE a --deploy-config file, not something resembling one.
func TestDeployLockLoadsBackAsADeployConfig(t *testing.T) {
	body, err := deployLockBytes(aburridesSettings().toDeployConfig(), "aburrides-73aa7738")
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	path := filepath.Join(t.TempDir(), deployLockName)
	if err := os.WriteFile(path, body, 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}

	cfg, err := loadDeployConfigFile(path)
	if err != nil {
		t.Fatalf("the lockfile does not load as a deploy config: %v", err)
	}

	// Every field that survives the strip must come back. The k8s ones are called
	// out because toDeployConfig omitted all nine, which made a snapshot of a k8s
	// deploy silently unrunnable — and, worse, one that carried `domain` while
	// dropping the other two hostnames, which is what makes helm delete a live
	// Ingress on replay.
	want := map[string]*string{
		"host":        cfg.Host,
		"domain":      cfg.Domain,
		"auth_domain": cfg.AuthDomain,
		"grpc_domain": cfg.GRPCDomain,
		"image_repo":  cfg.ImageRepo,
		"namespace":   cfg.Namespace,
		"release":     cfg.Release,
		"version":     cfg.Version,
		"connection":  cfg.Connection,
		"api":         cfg.API,
		"auth":        cfg.Auth,
	}
	for name, v := range want {
		if v == nil || *v == "" {
			t.Errorf("%s did not survive the round trip", name)
		}
	}
	if cfg.DBDSN != nil || cfg.SSHKey != nil || cfg.SourceDir != nil || cfg.WebURL != nil {
		t.Error("a stripped field came back through the loader")
	}
}

// The note is the first thing a reader sees, and it has to say the two things that
// stop the file being edited or misread.
func TestDeployLockExplainsItself(t *testing.T) {
	body, err := deployLockBytes(aburridesSettings().toDeployConfig(), "aburrides-73aa7738")
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	var lock struct {
		Nuzur deployLockNote `json:"_nuzur"`
	}
	if err := json.Unmarshal(body, &lock); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if !strings.Contains(lock.Nuzur.Generated, "DO NOT EDIT") {
		t.Errorf("the note does not warn that the file is rewritten: %q", lock.Nuzur.Generated)
	}
	if !strings.Contains(lock.Nuzur.Usage, "--deploy-config") {
		t.Errorf("the note does not say how to use the file: %q", lock.Nuzur.Usage)
	}
	if lock.Nuzur.DeploymentID != "aburrides-73aa7738" {
		t.Errorf("deployment id = %q, want the one this deploy ran as", lock.Nuzur.DeploymentID)
	}
}

// Where the file goes decides whether it can be committed at all.
func TestDeployLockPath(t *testing.T) {
	t.Run("the app dir, which on k8s is the git repo", func(t *testing.T) {
		st := &deployState{s: &deploySettings{}, appDir: "/w/nuzur-app/app", sourceRoot: "/w/nuzur-app"}
		want := filepath.Join("/w/nuzur-app/app", deployLockDir, deployLockName)
		if got := deployLockPath(st); got != want {
			t.Errorf("path = %q, want %q — the workspace root is the repo's PARENT, so a file there cannot be committed", got, want)
		}
	})

	t.Run("falls back to the source root", func(t *testing.T) {
		st := &deployState{s: &deploySettings{}, sourceRoot: "/w/nuzur-app"}
		if got := deployLockPath(st); got != filepath.Join("/w/nuzur-app", deployLockDir, deployLockName) {
			t.Errorf("path = %q", got)
		}
	})

	t.Run("a db-only deploy has no app to put it in", func(t *testing.T) {
		st := &deployState{s: &deploySettings{}, dbOnly: true, sourceRoot: "/w/nuzur-app"}
		if got := deployLockPath(st); got != "" {
			t.Errorf("path = %q, want none", got)
		}
	})

	t.Run("no directory at all writes nothing", func(t *testing.T) {
		if got := deployLockPath(&deployState{s: &deploySettings{}}); got != "" {
			t.Errorf("path = %q, want none", got)
		}
	})
}

// A bookkeeping file must never fail a deploy that worked.
func TestDeployLockWriteFailureDoesNotFailTheDeploy(t *testing.T) {
	dir := t.TempDir()
	// .nuzur occupied by a regular file: MkdirAll fails on this even as root,
	// unlike a permission bit.
	if err := os.WriteFile(filepath.Join(dir, deployLockDir), []byte("in the way"), 0o644); err != nil {
		t.Fatalf("seeding the obstruction: %v", err)
	}
	st := &deployState{s: aburridesSettings(), appDir: dir, depID: "aburrides-73aa7738"}

	if err := (&Implementation{}).stepWriteDeployLock(t.Context(), st); err != nil {
		t.Errorf("an unwritable lockfile failed the deploy: %v", err)
	}
	if st.deployLockPath != "" {
		t.Error("reported a path for a file it did not write")
	}
}

// A raw --db-dsn deploy writes nothing rather than something that looks runnable:
// replayed, a config with the DSN stripped would self-host a new empty database on
// the box, which is the one outcome the selector refuses outright.
func TestDeployLockSkippedForARawDSNDeploy(t *testing.T) {
	dir := t.TempDir()
	s := aburridesSettings()
	s.Connection = "" // --db-dsn only
	st := &deployState{s: s, appDir: dir, depID: "aburrides-73aa7738"}

	if err := (&Implementation{}).stepWriteDeployLock(t.Context(), st); err != nil {
		t.Fatalf("step: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, deployLockDir, deployLockName)); !os.IsNotExist(err) {
		t.Error("wrote a lockfile for a deploy whose credentials cannot be committed")
	}
}

// The happy path, end to end through the step.
func TestDeployLockIsWrittenAndReplayable(t *testing.T) {
	dir := t.TempDir()
	st := &deployState{s: aburridesSettings(), appDir: dir, depID: "aburrides-73aa7738"}

	if err := (&Implementation{}).stepWriteDeployLock(t.Context(), st); err != nil {
		t.Fatalf("step: %v", err)
	}
	path := filepath.Join(dir, deployLockDir, deployLockName)
	if st.deployLockPath != path {
		t.Errorf("recorded path = %q, want %q", st.deployLockPath, path)
	}
	cfg, err := loadDeployConfigFile(path)
	if err != nil {
		t.Fatalf("the written file does not load as a deploy config: %v", err)
	}
	if cfg.Host == nil || *cfg.Host != "69.164.192.246" {
		t.Error("the written file does not describe this deploy")
	}
	if advice := deployLockAdvice(st.deployLockPath); !strings.Contains(advice, "--deploy-config") {
		t.Errorf("the report does not tell the user how to replay it: %q", advice)
	}
}

// The cold-start property: a fresh clone, an agent with no memory of how this app
// was deployed, and nothing but the repository.
//
// source_dir is the one field the committed file cannot carry (it is an absolute
// path on whoever deployed last), and without it the deploy falls back to
// ./nuzur-<identifier> relative to the current directory — which is not the
// workspace, because the file lives in the APP dir and the workspace is its parent.
// Recovering it from the file's own location is what makes the replay work
// anywhere.
func TestLockfileRecoversItsWorkspaceFromItsOwnLocation(t *testing.T) {
	// <workspace>/<identifier>/.nuzur/last-deploy.json — the real layout.
	workspace := t.TempDir()
	appDir := filepath.Join(workspace, "aburrides")
	lock := filepath.Join(appDir, deployLockDir, deployLockName)

	t.Run("the workspace is the app dir's parent", func(t *testing.T) {
		cfg := &DeployConfig{}
		inferWorkspaceFromLockfile(cfg, lock)
		if cfg.SourceDir == nil {
			t.Fatal("no workspace inferred; a fresh clone would generate into the wrong directory")
		}
		if *cfg.SourceDir != workspace {
			t.Errorf("workspace = %q, want %q", *cfg.SourceDir, workspace)
		}
	})

	t.Run("a config that states its own workspace is left alone", func(t *testing.T) {
		stated := "/somewhere/else"
		cfg := &DeployConfig{SourceDir: &stated}
		inferWorkspaceFromLockfile(cfg, lock)
		if *cfg.SourceDir != stated {
			t.Errorf("workspace = %q, want the stated %q — a hand-written config must not be second-guessed", *cfg.SourceDir, stated)
		}
	})

	// THE case: the usage line the file prints is relative, run from the app dir.
	// Walking up a relative path bottoms out at "." after one step, so this is
	// where a guard against nonsense quietly disables the whole feature.
	t.Run("a relative path, exactly as the usage line prints it", func(t *testing.T) {
		if err := os.MkdirAll(filepath.Dir(lock), 0o755); err != nil {
			t.Fatalf("layout: %v", err)
		}
		cwd, err := os.Getwd()
		if err != nil {
			t.Fatalf("getwd: %v", err)
		}
		t.Cleanup(func() { _ = os.Chdir(cwd) })
		if err := os.Chdir(appDir); err != nil {
			t.Fatalf("chdir: %v", err)
		}

		cfg := &DeployConfig{}
		inferWorkspaceFromLockfile(cfg, filepath.Join(deployLockDir, deployLockName))
		if cfg.SourceDir == nil {
			t.Fatal("no workspace inferred from a relative lockfile path — the documented invocation would generate into the wrong directory")
		}
		// Compared through EvalSymlinks: macOS temp dirs are behind /private.
		got, _ := filepath.EvalSymlinks(*cfg.SourceDir)
		want, _ := filepath.EvalSymlinks(workspace)
		if got != want {
			t.Errorf("workspace = %q, want %q", got, want)
		}
	})

	t.Run("an ordinary deploy-config file infers nothing", func(t *testing.T) {
		cfg := &DeployConfig{}
		inferWorkspaceFromLockfile(cfg, filepath.Join(workspace, "prod.json"))
		if cfg.SourceDir != nil {
			t.Errorf("inferred %q from a file that is not a lockfile", *cfg.SourceDir)
		}
	})

	t.Run("no config file at all", func(t *testing.T) {
		cfg := &DeployConfig{}
		inferWorkspaceFromLockfile(cfg, "")
		if cfg.SourceDir != nil {
			t.Errorf("inferred %q from nothing", *cfg.SourceDir)
		}
	})
}

// The wiring, not just the rule: a lockfile passed as --deploy-config must reach
// the effective settings with its workspace already worked out. Testing the
// inference function alone would pass even if nothing ever called it.
func TestResolveDeploySettingsUsesTheLockfileWorkspace(t *testing.T) {
	workspace := t.TempDir()
	appDir := filepath.Join(workspace, "aburrides")
	path := filepath.Join(appDir, deployLockDir, deployLockName)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("layout: %v", err)
	}
	body, err := deployLockBytes(aburridesSettings().toDeployConfig(), "aburrides-73aa7738")
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	if err := os.WriteFile(path, body, 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}

	s, err := resolveDeploySettings(deployContext(t, []string{"--deploy-config", path}))
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if s.SourceDir != workspace {
		t.Errorf("source dir = %q, want the workspace the lockfile sits in (%q) — a fresh clone would generate into the wrong place", s.SourceDir, workspace)
	}
	// And the rest of the deploy came across, so the replay is a real one.
	if s.Host != "69.164.192.246" || s.GRPCDomain != "grpc.aburrides.com" || s.ImageRepo == "" {
		t.Errorf("the lockfile did not describe the deploy: host=%q grpc=%q repo=%q", s.Host, s.GRPCDomain, s.ImageRepo)
	}
	if s.DBDSN != "" {
		t.Error("a database password came back out of a committed file")
	}
}
