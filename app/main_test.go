package app

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/nuzur/nuzur-cli/files"
)

// realConfigDir is where this machine's deployment records actually live,
// captured before TestMain redirects HOME. Kept so the guard below can assert
// the isolation is in force rather than merely assume it.
var realConfigDir string

// TestMain redirects the per-user config directory for the whole package.
//
// files.DeploymentsDir() resolves through os.UserConfigDir(), which on darwin
// returns $HOME/Library/Application Support and ignores XDG_CONFIG_HOME
// completely — so setting XDG alone, as is enough on linux, isolates nothing on
// a Mac. Several tests here drive a deploy step far enough to checkpoint through
// MutateDeployment, and without this they wrote a record into the developer's
// own config directory, which `nuzur-cli deploy list` then read back as a real
// deployment. TestDeploymentStateRoundTrip in deploy/ sets both variables for
// exactly this reason; this is the same fix applied to the whole package, so it
// covers tests that reach the record layer indirectly and cannot be expected to
// know they do.
func TestMain(m *testing.M) {
	realConfigDir = files.DeploymentsDir()

	home, err := os.MkdirTemp("", "nuzur-cli-test-home")
	if err != nil {
		panic("creating a sandbox HOME for the tests: " + err.Error())
	}
	os.Setenv("HOME", home)
	os.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))

	code := m.Run()
	os.RemoveAll(home)
	os.Exit(code)
}

// The isolation above is invisible while it works, which is how it came to be
// missing in the first place: a leaked record is a stray file in a directory
// nobody looks at, not a failing test. This fails loudly if the redirect is
// weakened — dropping HOME for XDG alone, say, which still passes on linux CI
// while quietly writing to the developer's real config dir on macOS.
func TestDeploymentRecordsAreIsolatedFromTheRealConfigDir(t *testing.T) {
	dir := files.DeploymentsDir()
	if dir == realConfigDir {
		t.Fatalf("tests write deployment records to the real config dir %s — "+
			"records created here would show up in `nuzur-cli deploy list`", dir)
	}
	if home := os.Getenv("HOME"); !strings.HasPrefix(dir, home) {
		t.Errorf("deployment records go to %s, outside the sandbox HOME %s", dir, home)
	}
}
