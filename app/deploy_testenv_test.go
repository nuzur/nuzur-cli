package app

import (
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/nuzur/nuzur-cli/files"
	"github.com/nuzur/nuzur-cli/outputtools"
	"github.com/nuzur/nuzur-cli/productclient"
)

// fakeAuthToken is the login token isolateHome writes at files.TokenFilePath().
//
// productclient.ClientContext() does not parse it: it reads the file's bytes and
// puts them straight into the `authorization: bearer <bytes>` gRPC metadata (see
// productclient/client.go). So the only property that matters is that the file
// EXISTS and is non-empty — ClientContext's single failure mode is os.ReadFile
// erroring, which is what a test running against a pristine HOME hits at every
// one of its call sites ("building auth context: ...", and four more).
//
// It is still shaped like the real thing (an unsigned three-segment JWT, which is
// what auth.FetchToken persists — token.AccessToken verbatim) so that anything
// which later decides to inspect it finds a plausible value rather than "x", and
// it is a CONSTANT rather than a generated one so golden output stays byte-stable.
const fakeAuthToken = "eyJhbGciOiJSUzI1NiIsInR5cCI6IkpXVCJ9." +
	"eyJzdWIiOiJmODg4OGUzMy0wMDAwLTAwMDAtMDAwMC0wMDAwMDAwMDAwMDkiLCJwcmVmZXJyZWRfdXNlcm5hbWUiOiJkZXBsb3ktdGVzdHMiLCJlbWFpbCI6ImRlcGxveS10ZXN0c0BleGFtcGxlLmludmFsaWQiLCJleHAiOjQ4NzY1NDMyMTB9." +
	"c2lnbmF0dXJlLXBsYWNlaG9sZGVyLW5vdC12ZXJpZmllZC1ieS10aGUtY2xp"

// isolateHome points every per-user path the CLI writes — the deployments dir,
// the agent dir, the login token — at a temp directory for the duration of one
// test, and seeds the token file so productclient.ClientContext() succeeds.
//
// This is the app-package sibling of deploy.isolateDeploymentsDir
// (deploy/state_test.go): os.UserConfigDir reads HOME on darwin and
// XDG_CONFIG_HOME on unix, so both are set. It does the same skip-rather-than-
// pollute check, because a platform where UserConfigDir ignores both would
// otherwise have the test writing into the developer's real ~/.config/nuzur and
// deleting their real deployment records.
//
// Returns the temp dir so a caller can assert on (or normalize) paths under it.
//
// Tests using it must not call t.Parallel() — t.Setenv forbids it, and the
// process-wide HOME it swaps is exactly why.
func isolateHome(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("HOME", dir)
	t.Setenv("XDG_CONFIG_HOME", dir)

	if got := files.DeploymentsDir(); !pathUnder(got, dir) {
		t.Skipf("cannot isolate the deployments dir on this platform (got %s)", got)
	}
	tokenPath := files.TokenFilePath()
	if !pathUnder(tokenPath, dir) {
		t.Skipf("cannot isolate the token file on this platform (got %s)", tokenPath)
	}

	if err := os.MkdirAll(filepath.Dir(tokenPath), 0o755); err != nil {
		t.Fatalf("creating the isolated config dir: %v", err)
	}
	if err := os.WriteFile(tokenPath, []byte(fakeAuthToken), 0o600); err != nil {
		t.Fatalf("writing the isolated token file: %v", err)
	}
	return dir
}

// pathUnder reports whether path is inside dir.
func pathUnder(path, dir string) bool {
	return len(path) > len(dir) && strings.HasPrefix(path, dir)
}

// swapOutputWriters points the CLI's two output sinks at the given writers for
// the duration of one test, restoring them on cleanup.
//
// This is the capture half of the golden harness: every line the CLI authors
// goes through outputtools.Stdout or outputtools.Stderr (the colored helpers and
// the raw fmt.Fprint* sites alike), so swapping the two variables captures a
// whole command's output without plumbing a writer through forty signatures.
//
// ORDER MATTERS. The production SSH accessor SNAPSHOTS outputtools.Stderr into
// the runner it builds (Implementation.sshRunner sets r.Stderr = outputtools.Stderr
// at construction time), so a runner built before the swap keeps writing the
// box's live output to the real os.Stderr. Call this BEFORE building the
// Implementation under test — which is also the only order that captures
// anything the constructor itself prints.
//
// Callers must not t.Parallel(): the two variables are process-wide.
func swapOutputWriters(t *testing.T, out, err io.Writer) {
	t.Helper()
	prevOut, prevErr := outputtools.Stdout, outputtools.Stderr
	outputtools.Stdout, outputtools.Stderr = out, err
	t.Cleanup(func() {
		outputtools.Stdout, outputtools.Stderr = prevOut, prevErr
	})
}

// setArgv replaces os.Args for the duration of one test.
//
// It is part of the test ENVIRONMENT rather than a convenience: `deploy` reads
// os.Args directly — rerunCommand(os.Args, …) renders the "re-run with
// authorization" and "--plan" suggestions the destructive gate prints, and
// strayArgsError names the flag a stray positional belonged to. Under `go test`
// os.Args is the test binary's own command line (a path into the build cache
// plus -test.* flags), which is neither stable across machines nor anything a
// user would recognise, so a golden covering the gate would be unreproducible
// noise. Setting it to the invocation the scenario represents makes those lines
// exactly what the user would see.
func setArgv(t *testing.T, argv ...string) {
	t.Helper()
	prev := os.Args
	os.Args = argv
	t.Cleanup(func() { os.Args = prev })
}

// chdirTemp moves the process into dir for the duration of one test.
//
// A deploy with no --source-dir generates into ./nuzur-<identifier>, resolved
// against the working directory — so without this a golden test would write a
// generated workspace into the package source tree. Returns the directory as the
// process now sees it: os.Getwd() resolves symlinks on darwin (/var/folders/… is
// a symlink to /private/var/folders/…), so the path that ends up in the CLI's
// output is not necessarily the string that was passed in, and normalization
// needs the resolved form.
func chdirTemp(t *testing.T, dir string) string {
	t.Helper()
	prev, err := os.Getwd()
	if err != nil {
		t.Fatalf("reading the working directory: %v", err)
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("creating the working directory: %v", err)
	}
	if err := os.Chdir(dir); err != nil {
		t.Fatalf("changing to the working directory: %v", err)
	}
	t.Cleanup(func() { _ = os.Chdir(prev) })
	resolved, err := os.Getwd()
	if err != nil {
		t.Fatalf("reading back the working directory: %v", err)
	}
	return resolved
}

// The helper's whole contract: an isolated HOME the CLI's own path helpers agree
// with, and a token file good enough for every productclient.ClientContext()
// call on the deploy path. If either half regresses, the golden tests fail with
// "building auth context" instead of a diff, or — worse — pass while writing
// into the developer's real config dir.
func TestIsolateHome(t *testing.T) {
	dir := isolateHome(t)

	for name, got := range map[string]string{
		"deployments dir": files.DeploymentsDir(),
		"token file":      files.TokenFilePath(),
	} {
		if !pathUnder(got, dir) {
			t.Errorf("%s = %q, want a path under the temp HOME %q", name, got, dir)
		}
	}

	raw, err := os.ReadFile(files.TokenFilePath())
	if err != nil {
		t.Fatalf("token file not readable: %v", err)
	}
	if string(raw) != fakeAuthToken {
		t.Errorf("token file = %q, want the seeded token", raw)
	}

	// The reason the token file is seeded at all.
	if _, err := productclient.ClientContext(); err != nil {
		t.Fatalf("productclient.ClientContext() under an isolated HOME: %v", err)
	}
}
