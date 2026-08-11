package app

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	"github.com/nuzur/nuzur-cli/outputtools"
)

// The deploy lockfile: how this app was last deployed, written into the app's own
// repository so the answer travels with the code.
//
// Everything the CLI knew about a deployment used to live in one per-user file
// under ~/Library/Application Support (or ~/.config) — invisible, machine-local,
// and untransferable. A teammate, a fresh laptop or CI had no way to learn how an
// app deploys, and even on the machine that deployed it the record could only be
// selected by an id nothing prints next to the code.
//
// This is deliberately a LOCKFILE, not a config: regenerated wholesale after every
// successful deploy, never merged, never hand-edited. The distinction is the same
// one .helm/<id>/values-custom.yaml exists to draw — a file that is both generated
// and hand-edited eventually loses someone's edit. Anyone wanting a spec they
// maintain keeps their own file and passes it with --deploy-config.
//
// It is never read implicitly. Being simultaneously an input and a generated
// output is how a stale value silently comes back to life; you replay it by naming
// it, and the deploy prints the exact command.
const (
	deployLockDir  = ".nuzur"
	deployLockName = "last-deploy.json"
)

// deployLock is what lands on disk: a short note about what the file is, then the
// deploy config itself flattened alongside it (an embedded struct pointer inlines
// in encoding/json), so the file IS a --deploy-config file rather than something
// that has to be unwrapped first.
type deployLock struct {
	Nuzur *deployLockNote `json:"_nuzur"`
	*DeployConfig
}

type deployLockNote struct {
	Generated    string `json:"generated"`
	Usage        string `json:"usage"`
	DeploymentID string `json:"deployment_id,omitempty"`
}

// deployLockPath is where the lockfile goes, or "" when this deploy has no app
// directory to put it in (--db-only deploys generate no app at all).
//
// The APP dir, not the workspace root: on the k8s path the app dir is the git
// repository (the workspace root is its parent, which git does not know about), and
// a file outside the repo cannot be committed — which is the entire point.
func deployLockPath(st *deployState) string {
	dir := st.appDir
	if dir == "" {
		dir = st.sourceRoot
	}
	if st.dbOnly || dir == "" {
		return ""
	}
	return filepath.Join(dir, deployLockDir, deployLockName)
}

// withoutMachineLocal strips what must not travel: secrets, and paths that are
// true only on the machine that ran the deploy.
//
// Each removal is a thing that would actively mislead rather than merely bloat:
//   - db_dsn carries a database password, and this file is meant to be committed
//   - ssh_key is a path on one laptop; it also leaks a username
//   - source_dir is an absolute local path, and the file's own location already
//     says where the workspace is
//   - web_url is the operator's choice of endpoint (staging vs prod), not a
//     property of the app; pinning it would redirect a teammate's deploy
//
// The manual --s3-* credentials need no stripping: they are not DeployConfig
// fields at all, which is the existing secret boundary and stays untouched.
func (c *DeployConfig) withoutMachineLocal() *DeployConfig {
	if c == nil {
		return nil
	}
	out := *c
	out.DBDSN = nil
	out.SSHKey = nil
	out.SourceDir = nil
	out.WebURL = nil
	return &out
}

// deployLockBytes renders the file. Deterministic by construction: no timestamp,
// no CLI version, nothing that changes when the deploy did not.
//
// That is a requirement, not tidiness. On the k8s path `git add` is scoped to the
// app dir and runs on every deploy, so a file carrying a timestamp would produce a
// commit every single run — and a commit triggers CI, which builds an image, which
// is a multi-minute round trip caused entirely by bookkeeping.
func deployLockBytes(cfg *DeployConfig, deploymentID string) ([]byte, error) {
	lock := deployLock{
		Nuzur: &deployLockNote{
			Generated: "nuzur-cli deploy — rewritten after every successful deploy. DO NOT EDIT.",
			Usage:     "nuzur-cli deploy --deploy-config " + deployLockDir + "/" + deployLockName,
			// Informational only. Record ids are machine-local, so nothing consumes
			// this — but it is the answer to "which of these recorded deployments is
			// this app", stored where a teammate can read it.
			DeploymentID: deploymentID,
		},
		DeployConfig: cfg.withoutMachineLocal(),
	}
	body, err := json.MarshalIndent(lock, "", "  ")
	if err != nil {
		return nil, err
	}
	return append(body, '\n'), nil
}

// stepWriteDeployLock records how this deploy ran, into the app's repository.
//
// Placed after `finalize record`, which is the point at which the deploy is known
// to have shipped: the box is recorded, the release is applied, the front door has
// been read back and the record's error cleared. Writing earlier would commit a
// description of a deploy that never happened.
//
// Never fails the deploy. A read-only workspace, a .nuzur path occupied by a
// regular file, a full disk — none of those are reasons to fail a deploy that
// worked, and this pipeline does not make that trade anywhere else.
func (i *Implementation) stepWriteDeployLock(ctx context.Context, st *deployState) error {
	path := deployLockPath(st)
	if path == "" {
		return nil
	}
	// A raw --db-dsn deploy gets no lockfile at all. Stripping the DSN would leave
	// a file that LOOKS runnable and, replayed, would self-host a brand-new empty
	// database on the box — the one outcome applyDeploymentSelector refuses
	// outright. Better to write nothing and say why.
	if st.s.DBDSN != "" && st.s.Connection == "" {
		outputtools.PrintlnColoredErr(
			"No "+deployLockDir+"/"+deployLockName+" written: this deploy used --db-dsn, whose credentials must not be committed. "+
				"Deploy with --connection <uuid> to get a shareable config.", outputtools.Yellow)
		return nil
	}

	body, err := deployLockBytes(st.s.toDeployConfig(), st.depID)
	if err != nil {
		outputtools.PrintlnColoredErr("Could not render "+deployLockName+" (continuing): "+err.Error(), outputtools.Yellow)
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		outputtools.PrintlnColoredErr("Could not write "+deployLockName+" (continuing): "+err.Error(), outputtools.Yellow)
		return nil
	}
	// Temp + rename, for the same reason the record does it: a write interrupted
	// midway would otherwise leave a truncated file that still parses as a
	// different, wrong deploy.
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, body, 0o644); err != nil {
		outputtools.PrintlnColoredErr("Could not write "+deployLockName+" (continuing): "+err.Error(), outputtools.Yellow)
		return nil
	}
	if err := os.Rename(tmp, path); err != nil {
		os.Remove(tmp)
		outputtools.PrintlnColoredErr("Could not write "+deployLockName+" (continuing): "+err.Error(), outputtools.Yellow)
		return nil
	}
	st.deployLockPath = path
	return nil
}

// deployLockAdvice is the line the report prints, or "" when no file was written.
func deployLockAdvice(path string) string {
	if path == "" {
		return ""
	}
	rel := filepath.Join(deployLockDir, deployLockName)
	return fmt.Sprintf("  Deploy config: %s — commit it; `nuzur-cli deploy --deploy-config %s` repeats this deploy.\n"+
		"  (Rather not have the server address in git? add %s/ to .gitignore.)",
		rel, rel, deployLockDir)
}

// inferWorkspaceFromLockfile fills in the workspace from the lockfile's location,
// when the config came from a lockfile and states no source_dir of its own.
//
// The committed file cannot carry source_dir — it is an absolute path on whoever
// deployed last — but it does not need to: the file is written to
// <appDir>/.nuzur/last-deploy.json, and the workspace root is <appDir>'s parent.
// Reading it back off the path is correct on every machine, which is the whole
// difference between "a config you still have to know things to use" and one a
// fresh clone can run.
//
// Only for a file named like a lockfile, and only when source_dir is unset: a
// hand-maintained --deploy-config that says where its workspace is must be left
// exactly as written.
func inferWorkspaceFromLockfile(cfg *DeployConfig, configPath string) {
	if cfg == nil || configPath == "" || cfg.SourceDir != nil {
		return
	}
	// Absolute FIRST, then walk up. The usage line this file prints is a RELATIVE
	// path — `--deploy-config .nuzur/last-deploy.json`, run from the app dir — and
	// walking up a relative path bottoms out at "." after one step, which reads as
	// nonsense and would skip the inference on exactly the invocation it exists to
	// serve.
	abs, err := filepath.Abs(configPath)
	if err != nil {
		return
	}
	dir, name := filepath.Split(filepath.Clean(abs))
	if name != deployLockName || filepath.Base(filepath.Clean(dir)) != deployLockDir {
		return
	}
	appDir := filepath.Dir(filepath.Clean(dir))
	workspace := filepath.Dir(appDir)
	// Only a real ancestor: at the filesystem root Dir is its own parent, and a
	// workspace equal to the app dir would generate into itself.
	if workspace == appDir || workspace == "" || workspace == "." {
		return
	}
	cfg.SourceDir = &workspace
}
