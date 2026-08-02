package main

// installer_test.go tests install.sh — the script behind
// `curl -fsSL https://nuzur.com/install.sh | sh`.
//
// It is tested the way the deploy bootstrap is (deploy/bootstrap_apt_test.go):
// parsed by every shell that will run it, and then actually RUN against stub
// tools, because reading shell is not evidence. The script's whole job is to
// decide — which OS, which architecture, which version, which directory, and
// whether the bytes it downloaded are the bytes that were published — and every
// one of those decisions is invisible until something executes it.
//
// Nothing here touches the network or the machine's real directories: `curl` and
// `uname` are stubs on a controlled PATH, HOME is a temp dir, and the "release"
// is a tarball this file builds and checksums itself.

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/nuzur/nuzur-cli/deploy"
)

const installScriptName = "install.sh"

// The markers install.sh carries so a block of it can be lifted out and run on
// its own. They are comments in the script and constants here; changing one
// without the other fails loudly rather than silently testing nothing.
const (
	installHelpersStart = "# ── helpers ─"
	installHelpersEnd   = "# ── end helpers ─"
	installDestStart    = "# ── dest resolution ─"
	installDestEnd      = "# ── end dest resolution ─"
)

// installScript is the script under test, read from the repo root.
func installScript(t *testing.T) string {
	t.Helper()
	b, err := os.ReadFile(installScriptName)
	if err != nil {
		t.Fatalf("reading %s: %v", installScriptName, err)
	}
	return string(b)
}

func installScriptPath(t *testing.T) string {
	t.Helper()
	p, err := filepath.Abs(installScriptName)
	if err != nil {
		t.Fatal(err)
	}
	return p
}

// ─────────────────────────────────────────────────────────────────────────────
// 1. It has to BE a script — in every shell that will run it.
// ─────────────────────────────────────────────────────────────────────────────

// The advertised one-liner is `| sh`, and `sh` is dash on Debian/Ubuntu and
// busybox ash in most containers. A bash-ism does not degrade there, it fails
// outright — so the script is parsed by /bin/sh AND by bash, and by dash itself
// wherever dash exists (on ubuntu-latest, where CI runs, /bin/sh IS dash, so the
// first check is already the strict one).
func TestInstallScriptIsValidShell(t *testing.T) {
	path := installScriptPath(t)
	for _, shell := range []string{"/bin/sh", "bash", "dash", "busybox"} {
		bin, err := exec.LookPath(shell)
		if err != nil {
			continue
		}
		t.Run(filepath.Base(shell), func(t *testing.T) {
			args := []string{"-n", path}
			if filepath.Base(shell) == "busybox" {
				args = append([]string{"sh"}, args...)
			}
			if out, err := exec.Command(bin, args...).CombinedOutput(); err != nil {
				t.Errorf("%s -n %s: %v\n%s", bin, installScriptName, err, out)
			}
		})
	}

	script := installScript(t)
	if !strings.HasPrefix(script, "#!/bin/sh\n") {
		t.Error("install.sh must start with #!/bin/sh — the one-liner pipes into sh, not bash")
	}
	if !strings.Contains(script, "\nset -eu\n") {
		t.Error("install.sh must `set -eu`: an unchecked failure here installs a broken binary")
	}
	// pipefail is not POSIX. dash and busybox ash abort on `set -o pipefail`, which
	// would make the script fail on exactly the machines it exists for. (The
	// script's own comment says so, hence the specific string rather than the word.)
	if strings.Contains(script, "set -o pipefail") {
		t.Error("install.sh sets pipefail, which dash and busybox ash do not have")
	}
	// Bash-isms that `sh -n` on macOS will happily parse — /bin/sh there is bash in
	// posix mode, so the syntax check is only strict where dash exists. These are
	// the ones that would actually break on dash. Comments are stripped first: the
	// script's own header documents the ban, and matching that would be the test
	// failing on its own subject matter.
	var code strings.Builder
	for _, line := range strings.Split(script, "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "#") {
			continue
		}
		code.WriteString(line)
		code.WriteString("\n")
	}
	for _, banned := range []string{"[[ ", "$'", "<<<", "declare ", "local ", "function "} {
		if strings.Contains(code.String(), banned) {
			t.Errorf("install.sh contains the bash-ism %q — dash and busybox ash run this script", banned)
		}
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// 2. Drift lock: the script, the bootstrap and the probe fetch the SAME files.
// ─────────────────────────────────────────────────────────────────────────────

// Three things compose the release URLs: this script (both values resolved from
// `uname`), the deploy bootstrap (Linux, arch resolved on the box) and the Go
// pre-flight probe. Nothing but a test makes them the same string, and the cost
// of drift is a check that reports confidently about a file nobody downloads.
//
// The trick is to ask the Go helpers for the URL using the SHELL's own
// placeholders as arguments, so their output is literally the line the script
// must contain. A hard-coded literal in either place then fails here rather than
// on someone's machine.
func TestInstallScriptComposesTheReleaseURLs(t *testing.T) {
	script := installScript(t)

	wantAsset := deploy.CLIReleaseAssetURL("${NUZUR_VERSION}", "${NUZUR_OS}", "${NUZUR_ARCH}")
	if !strings.Contains(script, wantAsset) {
		t.Errorf("install.sh does not compose deploy.CLIReleaseAssetURL's output.\n"+
			"  want the script to download from: %s\n"+
			"install.sh and deploy.CLIReleaseAssetURL have drifted — the installer now\n"+
			"fetches a different file from the one the deploy bootstrap and the release\n"+
			"probe talk about.", wantAsset)
	}

	wantSums := deploy.CLIReleaseChecksumsURL("${NUZUR_VERSION}")
	if !strings.Contains(script, wantSums) {
		t.Errorf("install.sh does not compose deploy.CLIReleaseChecksumsURL's output.\n"+
			"  want the script to verify against: %s\n"+
			"Note the version appears TWICE and in two forms — with the `v` in the tag\n"+
			"segment, without it in goreleaser's checksums filename.", wantSums)
	}

	// And the same line, with real values substituted, is byte-for-byte the URL the
	// bootstrap downloads and the probe HEADs. This is the claim the placeholder
	// assertion above only implies.
	subst := strings.NewReplacer(
		"${NUZUR_VERSION}", "1.6.1",
		"${NUZUR_OS}", deploy.CLIReleaseOSLinux,
		"${NUZUR_ARCH}", deploy.CLIReleaseArchX8664,
	)
	if got, want := subst.Replace(wantAsset), deploy.CLIReleaseAssetURL("1.6.1", deploy.CLIReleaseOSLinux, deploy.CLIReleaseArchX8664); got != want {
		t.Errorf("the installer's Linux/x86_64 URL is not the bootstrap's:\n got: %s\nwant: %s", got, want)
	}

	// The checksums filename carries the version WITHOUT the leading v. Verified
	// against the live release: nuzur-cli_1.6.1_checksums.txt is the published name.
	if got := deploy.CLIReleaseChecksumsURL("v1.6.1"); !strings.HasSuffix(got, "/v1.6.1/nuzur-cli_1.6.1_checksums.txt") {
		t.Errorf("CLIReleaseChecksumsURL(%q) = %q — the tag segment keeps the v, the filename drops it", "v1.6.1", got)
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// The harness.
// ─────────────────────────────────────────────────────────────────────────────

// installCfg describes one scripted world for the installer to run in: what
// `uname` says, what "GitHub" serves, and what the environment offers.
type installCfg struct {
	unameS string // uname -s; "" ⇒ Linux
	unameM string // uname -m; "" ⇒ x86_64

	latestTag string // tag_name the API answers with; "" ⇒ v1.6.1
	apiFails  bool   // the API URL is not served at all (curl exits 22)

	pin       string // NUZUR_VERSION, "" ⇒ unset
	dest      string // NUZUR_INSTALL_DIR, "" ⇒ unset (dest resolution decides)
	home      string // HOME, "" ⇒ a temp dir
	systemBin string // NUZUR_SYSTEM_BIN, "" ⇒ a temp dir that does NOT exist

	noAsset         bool // the release tarball 404s
	noChecksums     bool // the checksums file 404s
	badChecksum     bool // the checksums file lists a different hash
	noChecksumEntry bool // the checksums file has no line for this asset

	// utilities is the PATH tail: the directories holding the real tools the
	// script uses. nil ⇒ /usr/bin:/bin, which is what a normal machine has.
	utilities []string
	// pathExtra are directories placed on PATH after the stub dir — used to make
	// a destination "already on PATH".
	pathExtra []string

	env []string // extra environment entries
}

type installRun struct {
	stdout, stderr, out string
	exit                int
	urls                []string // every URL the stub curl was asked for, in order
	dest                string   // NUZUR_INSTALL_DIR as passed (may be "")
	home                string
	version             string // the version the world publishes as latest, bare
}

// installArch is the script's `uname -m` → goreleaser map, mirrored so a test can
// name the asset it expects to be fetched.
func installArch(unameM string) string {
	switch unameM {
	case "x86_64", "amd64":
		return "x86_64"
	case "aarch64", "arm64":
		return "arm64"
	case "i386", "i686":
		return "i386"
	}
	return ""
}

// fakeCLITarball is a release archive: the binary the script installs plus the
// LICENSE and README goreleaser ships beside it. Those two are here on purpose —
// they are what makes the script's single-member `tar -xzf ... nuzur-cli` a claim
// worth testing rather than a stylistic choice.
//
// The "binary" is a shell script so that an arm64 test can run on an x86 host and
// a Linux test can run on macOS: what is being tested is the installer, not the
// CLI.
func fakeCLITarball(t *testing.T, version string) []byte {
	t.Helper()
	bin := "#!/bin/sh\n" +
		"case \"${1:-}\" in\n" +
		"--version|-v) printf 'nuzur-cli version " + version + "\\n' ;;\n" +
		"*) printf 'fake nuzur-cli " + version + "\\n' ;;\n" +
		"esac\n"
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	tw := tar.NewWriter(zw)
	for _, f := range []struct {
		name string
		body string
		mode int64
	}{
		{"LICENSE", "MIT\n", 0o644},
		{"README.md", "# nuzur-cli\n", 0o644},
		{"nuzur-cli", bin, 0o755},
	} {
		if err := tw.WriteHeader(&tar.Header{
			Name: f.name, Mode: f.mode, Size: int64(len(f.body)), Typeflag: tar.TypeReg,
		}); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write([]byte(f.body)); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// utilityDir builds a bin directory of symlinks to the host tools the script and
// the stubs need, minus `omit`.
//
// It exists for one reason: PATH can SHADOW a tool but it cannot HIDE one, and
// the sha256 fallback is precisely a question about a tool being absent. Handing
// the script a PATH that contains only what this returns is the only way to ask
// it.
func utilityDir(t *testing.T, omit ...string) string {
	t.Helper()
	skip := map[string]bool{}
	for _, o := range omit {
		skip[o] = true
	}
	dir := t.TempDir()
	// Everything install.sh, the stub curl and the stub uname reach for.
	// gzip is not invoked by the script itself but IS exec'd by GNU tar for -z
	// (macOS bsdtar has gzip built in, so the omission only surfaced on Linux
	// CI: "tar (child): gzip: Cannot exec").
	for _, name := range []string{
		"cat", "cp", "cut", "grep", "gzip", "head", "id", "install", "ln",
		"mkdir", "mktemp", "rm", "sed", "tar", "sha256sum", "shasum",
	} {
		if skip[name] {
			continue
		}
		src, err := exec.LookPath(name)
		if err != nil {
			continue // absent on this host; the script's own fallbacks decide
		}
		if err := os.Symlink(src, filepath.Join(dir, name)); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

// runInstall executes install.sh in a scripted world and reports what it did.
func runInstall(t *testing.T, cfg installCfg) installRun {
	t.Helper()

	if cfg.unameS == "" {
		cfg.unameS = "Linux"
	}
	if cfg.unameM == "" {
		cfg.unameM = "x86_64"
	}
	if cfg.latestTag == "" {
		cfg.latestTag = "v1.6.1"
	}
	base := t.TempDir()
	if cfg.home == "" {
		cfg.home = filepath.Join(base, "home")
		if err := os.MkdirAll(cfg.home, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if cfg.systemBin == "" {
		// Never the real /usr/local/bin. A test that installed there would be
		// writing to the developer's machine.
		cfg.systemBin = filepath.Join(base, "no-such-system-bin")
	}
	if cfg.utilities == nil {
		cfg.utilities = []string{"/usr/bin", "/bin"}
	}

	// What the world publishes, and what the script will therefore ask for.
	latest := strings.TrimPrefix(cfg.latestTag, "v")
	wanted := latest
	if cfg.pin != "" {
		wanted = strings.TrimPrefix(cfg.pin, "v")
	}
	arch := installArch(cfg.unameM)
	assetName := fmt.Sprintf("nuzur-cli_%s_%s.tar.gz", cfg.unameS, arch)

	serveDir := filepath.Join(base, "release")
	if err := os.MkdirAll(serveDir, 0o755); err != nil {
		t.Fatal(err)
	}
	write := func(name string, body []byte) string {
		p := filepath.Join(serveDir, name)
		if err := os.WriteFile(p, body, 0o644); err != nil {
			t.Fatal(err)
		}
		return p
	}

	tarball := fakeCLITarball(t, wanted)
	sum := sha256.Sum256(tarball)
	listed := hex.EncodeToString(sum[:])
	if cfg.badChecksum {
		listed = strings.Repeat("0", 64)
	}
	// goreleaser's format, with the siblings present so the grep is selecting
	// rather than reading the only line in the file.
	var sums strings.Builder
	for _, other := range []string{
		"nuzur-cli_Darwin_arm64.tar.gz", "nuzur-cli_Darwin_x86_64.tar.gz",
		"nuzur-cli_Linux_arm64.tar.gz", "nuzur-cli_Linux_i386.tar.gz",
		"nuzur-cli_Linux_x86_64.tar.gz", "nuzur-cli_Windows_x86_64.zip",
	} {
		if other == assetName {
			continue
		}
		fmt.Fprintf(&sums, "%s  %s\n", strings.Repeat("a", 64), other)
	}
	if !cfg.noChecksumEntry && arch != "" {
		fmt.Fprintf(&sums, "%s  %s\n", listed, assetName)
	}

	apiURL := "https://api.github.com/repos/nuzur/nuzur-cli/releases/latest"
	serve := map[string]string{}
	if !cfg.apiFails {
		serve[apiURL] = write("latest.json", []byte(fmt.Sprintf(
			"{\n  \"url\": \"https://api.github.com/repos/nuzur/nuzur-cli/releases/1\",\n  \"tag_name\": \"%s\",\n  \"name\": \"%s\"\n}\n",
			cfg.latestTag, cfg.latestTag)))
	}
	if arch != "" {
		if !cfg.noAsset {
			serve[deploy.CLIReleaseAssetURL(wanted, cfg.unameS, arch)] = write(assetName, tarball)
		}
		if !cfg.noChecksums {
			serve[deploy.CLIReleaseChecksumsURL(wanted)] = write("checksums.txt", []byte(sums.String()))
		}
	}

	stub := filepath.Join(base, "stub")
	if err := os.MkdirAll(stub, 0o755); err != nil {
		t.Fatal(err)
	}
	urlLog := filepath.Join(base, "urls.log")

	// The stub curl: logs every URL it is asked for, serves the mapped file to -o
	// or to stdout, and exits 22 (curl's own "HTTP error" code) for anything else —
	// which is exactly what a real 404 under -f looks like to the script.
	var curl strings.Builder
	curl.WriteString("#!/bin/sh\nurl=''\nout=''\n" +
		"while [ $# -gt 0 ]; do\n" +
		"  case \"$1\" in\n" +
		"    -o) out=\"$2\"; shift 2 ;;\n" +
		"    http*) url=\"$1\"; shift ;;\n" +
		"    *) shift ;;\n" +
		"  esac\n" +
		"done\n" +
		"printf '%s\\n' \"$url\" >> '" + urlLog + "'\n" +
		"case \"$url\" in\n")
	for u, f := range serve {
		curl.WriteString("  '" + u + "') src='" + f + "' ;;\n")
	}
	curl.WriteString("  *) exit 22 ;;\nesac\n" +
		"if [ -n \"$out\" ]; then cp \"$src\" \"$out\"; else cat \"$src\"; fi\n")
	writeStub(t, stub, "curl", curl.String())

	writeStub(t, stub, "uname", "#!/bin/sh\ncase \"${1:-}\" in\n"+
		"  -m) printf '%s\\n' '"+cfg.unameM+"' ;;\n"+
		"  *) printf '%s\\n' '"+cfg.unameS+"' ;;\n"+
		"esac\n")

	pathDirs := append([]string{stub}, cfg.pathExtra...)
	pathDirs = append(pathDirs, cfg.utilities...)

	env := []string{
		"PATH=" + strings.Join(pathDirs, string(os.PathListSeparator)),
		"HOME=" + cfg.home,
		"TMPDIR=" + base,
		"NUZUR_SYSTEM_BIN=" + cfg.systemBin,
	}
	if cfg.pin != "" {
		env = append(env, "NUZUR_VERSION="+cfg.pin)
	}
	if cfg.dest != "" {
		env = append(env, "NUZUR_INSTALL_DIR="+cfg.dest)
	}
	env = append(env, cfg.env...)

	cmd := exec.Command("/bin/sh", installScriptPath(t))
	cmd.Env = env
	cmd.Dir = base
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	err := cmd.Run()

	run := installRun{
		stdout:  stdout.String(),
		stderr:  stderr.String(),
		exit:    0,
		dest:    cfg.dest,
		home:    cfg.home,
		version: wanted,
	}
	run.out = run.stdout + run.stderr
	if err != nil {
		var ee *exec.ExitError
		if !errorsAs(err, &ee) {
			t.Fatalf("running install.sh: %v\n%s", err, run.out)
		}
		run.exit = ee.ExitCode()
	}
	if raw, rerr := os.ReadFile(urlLog); rerr == nil {
		for _, line := range strings.Split(strings.TrimSpace(string(raw)), "\n") {
			if line != "" {
				run.urls = append(run.urls, line)
			}
		}
	}
	return run
}

// errorsAs keeps the import list short for one use.
func errorsAs(err error, target **exec.ExitError) bool {
	ee, ok := err.(*exec.ExitError)
	if ok {
		*target = ee
	}
	return ok
}

func writeStub(t *testing.T, dir, name, body string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
}

// mustContain is the shape most of these assertions take: a message is only
// useful if it says the specific things a reader needs.
func mustContain(t *testing.T, what, got string, wants ...string) {
	t.Helper()
	for _, w := range wants {
		if !strings.Contains(got, w) {
			t.Errorf("%s does not mention %q:\n%s", what, w, got)
		}
	}
}

func dirEntries(t *testing.T, dir string) []string {
	t.Helper()
	ents, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		t.Fatal(err)
	}
	var names []string
	for _, e := range ents {
		names = append(names, e.Name())
	}
	return names
}

// ─────────────────────────────────────────────────────────────────────────────
// 3. The happy path, on every OS/arch that has an asset.
// ─────────────────────────────────────────────────────────────────────────────

// One machine can only test its own OS and architecture for real; the stubs let
// all five supported combinations be run here. What each asserts is that the
// script asked GitHub for the ONE file goreleaser publishes for that pair — the
// spellings differ between `uname -m` and the asset name for every architecture,
// and a wrong mapping is a 404 the user cannot do anything about.
func TestInstallScriptInstallsPerOSArch(t *testing.T) {
	for _, tc := range []struct {
		unameS, unameM, wantAsset string
	}{
		{"Linux", "x86_64", "nuzur-cli_Linux_x86_64.tar.gz"},
		{"Linux", "aarch64", "nuzur-cli_Linux_arm64.tar.gz"},
		{"Linux", "i686", "nuzur-cli_Linux_i386.tar.gz"},
		{"Darwin", "arm64", "nuzur-cli_Darwin_arm64.tar.gz"},
		{"Darwin", "amd64", "nuzur-cli_Darwin_x86_64.tar.gz"},
	} {
		t.Run(tc.unameS+"/"+tc.unameM, func(t *testing.T) {
			dest := filepath.Join(t.TempDir(), "bin")
			r := runInstall(t, installCfg{unameS: tc.unameS, unameM: tc.unameM, dest: dest})
			if r.exit != 0 {
				t.Fatalf("exit = %d, want 0\n%s", r.exit, r.out)
			}
			mustContain(t, "the run", r.out, tc.wantAsset, "checksum verified", "installed", "nuzur-cli version 1.6.1")

			// The asset URL, exactly.
			want := "https://github.com/nuzur/nuzur-cli/releases/download/v1.6.1/" + tc.wantAsset
			found := false
			for _, u := range r.urls {
				if u == want {
					found = true
				}
			}
			if !found {
				t.Errorf("the script never fetched %s; it fetched %v", want, r.urls)
			}

			// Only the binary and its alias land in the destination — the archive's
			// LICENSE and README stay in the tarball.
			if got := dirEntries(t, dest); len(got) != 2 {
				t.Errorf("destination holds %v, want exactly nuzur-cli and nuzur", got)
			}
			out, err := exec.Command(filepath.Join(dest, "nuzur-cli"), "--version").CombinedOutput()
			if err != nil {
				t.Fatalf("the installed binary does not run: %v\n%s", err, out)
			}
			if !strings.Contains(string(out), "1.6.1") {
				t.Errorf("installed binary reports %q", out)
			}
		})
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// 4. The refusals.
// ─────────────────────────────────────────────────────────────────────────────

// Git Bash / MSYS / Cygwin are POSIX shells on a machine whose nuzur-cli release
// is a .zip — there is no tarball to install. The refusal is the in-tool
// documentation of the Windows story, so it has to name Scoop and the page that
// lists every option; a bare "unsupported" would leave a Windows user with
// nothing to do next.
func TestInstallScriptRejectsUnsupportedOS(t *testing.T) {
	for _, osName := range []string{"MINGW64_NT-10.0-22631", "MSYS_NT-10.0", "CYGWIN_NT-10.0", "FreeBSD", "SunOS"} {
		t.Run(osName, func(t *testing.T) {
			dest := filepath.Join(t.TempDir(), "bin")
			r := runInstall(t, installCfg{unameS: osName, dest: dest})
			if r.exit == 0 {
				t.Fatalf("exit = 0 on %s, want a refusal\n%s", osName, r.out)
			}
			mustContain(t, "the refusal", r.stderr, osName, "scoop", "https://nuzur.com/cli")
			if len(r.urls) != 0 {
				t.Errorf("an unsupported OS still reached the network: %v", r.urls)
			}
			if got := dirEntries(t, dest); len(got) != 0 {
				t.Errorf("an unsupported OS still wrote %v", got)
			}
		})
	}
}

// Apple dropped 32-bit execution entirely, so goreleaser publishes no
// Darwin/i386 asset. Detected here rather than three steps later as a 404 with a
// URL nobody can act on.
func TestInstallScriptRejectsDarwinI386(t *testing.T) {
	r := runInstall(t, installCfg{unameS: "Darwin", unameM: "i386", dest: filepath.Join(t.TempDir(), "bin")})
	if r.exit == 0 {
		t.Fatalf("Darwin/i386 installed something\n%s", r.out)
	}
	mustContain(t, "the refusal", r.stderr, "32-bit macOS", "https://nuzur.com/cli")
	if len(r.urls) != 0 {
		t.Errorf("Darwin/i386 still reached the network: %v", r.urls)
	}
}

func TestInstallScriptRejectsUnknownArch(t *testing.T) {
	for _, arch := range []string{"ppc64le", "riscv64", "mips"} {
		t.Run(arch, func(t *testing.T) {
			r := runInstall(t, installCfg{unameM: arch, dest: filepath.Join(t.TempDir(), "bin")})
			if r.exit == 0 {
				t.Fatalf("%s installed something\n%s", arch, r.out)
			}
			mustContain(t, "the refusal", r.stderr, arch, "x86_64", "arm64", "https://nuzur.com/cli")
		})
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// 5. Version resolution.
// ─────────────────────────────────────────────────────────────────────────────

// A pin is an instruction, not a hint: with NUZUR_VERSION set the script must not
// consult the API at all. This matters beyond tidiness — CI runners and
// containers hit GitHub's unauthenticated rate limit, and a pinned install that
// still asked "what is latest?" would fail for a reason that has nothing to do
// with the version it was told to install.
//
// The world here deliberately publishes v9.9.9 as latest and serves assets ONLY
// for the pin, so a script that consulted the API would also fail loudly.
func TestInstallScriptPinnedVersionSkipsTheAPI(t *testing.T) {
	for _, pin := range []string{"v1.6.1", "1.6.1"} {
		t.Run(pin, func(t *testing.T) {
			dest := filepath.Join(t.TempDir(), "bin")
			r := runInstall(t, installCfg{pin: pin, latestTag: "v9.9.9", dest: dest})
			if r.exit != 0 {
				t.Fatalf("exit = %d, want 0\n%s", r.exit, r.out)
			}
			for _, u := range r.urls {
				if strings.Contains(u, "api.github.com") {
					t.Errorf("a pinned install consulted the API: %v", r.urls)
				}
			}
			// Both spellings of the pin resolve to the same tag: the `v` is stripped
			// on the way in and re-added by the URL.
			mustContain(t, "the run", r.out, "v1.6.1", "installed")
			for _, u := range r.urls {
				if strings.Contains(u, "9.9.9") {
					t.Errorf("a pinned install fetched the latest release instead: %s", u)
				}
			}
		})
	}
}

// Unpinned, the version comes from the API's tag_name — NOT from
// `releases/latest/download/...`, which resolves at curl time to whatever Release
// exists at that instant. A Release exists from the moment it is created, seconds
// before goreleaser has uploaded any assets, so that redirect hands out 404s
// during every release window. Asking for tag_name and then fetching that tag
// keeps the resolve and the download talking about one release.
func TestInstallScriptResolvesLatestFromTheAPI(t *testing.T) {
	dest := filepath.Join(t.TempDir(), "bin")
	r := runInstall(t, installCfg{latestTag: "v2.4.0", dest: dest})
	if r.exit != 0 {
		t.Fatalf("exit = %d, want 0\n%s", r.exit, r.out)
	}
	if len(r.urls) == 0 || r.urls[0] != "https://api.github.com/repos/nuzur/nuzur-cli/releases/latest" {
		t.Fatalf("the first request was not the releases API: %v", r.urls)
	}
	for _, u := range r.urls[1:] {
		if !strings.Contains(u, "/download/v2.4.0/") {
			t.Errorf("a request did not use the resolved tag v2.4.0: %s", u)
		}
		if strings.Contains(u, "releases/latest/download") {
			t.Errorf("the script used the unpinned latest redirect: %s", u)
		}
	}
	mustContain(t, "the run", r.out, "latest release is v2.4.0", "nuzur-cli version 2.4.0")
}

// When the API cannot be reached the script has nothing to install and must say
// so in a way the reader can act on: the URL it tried, the pin that gets past it,
// and where the versions are listed.
func TestInstallScriptLatestFailureNamesTheAPIURL(t *testing.T) {
	dest := filepath.Join(t.TempDir(), "bin")
	r := runInstall(t, installCfg{apiFails: true, dest: dest})
	if r.exit == 0 {
		t.Fatalf("an unreachable API still installed something\n%s", r.out)
	}
	mustContain(t, "the failure", r.stderr,
		"https://api.github.com/repos/nuzur/nuzur-cli/releases/latest",
		"NUZUR_VERSION=",
		"https://github.com/nuzur/nuzur-cli/releases",
	)
	if got := dirEntries(t, dest); len(got) != 0 {
		t.Errorf("a failed resolve still wrote %v", got)
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// 6. Verification: the reason this script downloads two files instead of one.
// ─────────────────────────────────────────────────────────────────────────────

// The bytes that arrive are not necessarily the bytes that were published. On a
// mismatch the ONLY acceptable outcome is that nothing is installed — and the
// message has to name both URLs, because "which of these two is wrong" is the
// question the reader is left with.
func TestInstallScriptChecksumMismatchFails(t *testing.T) {
	dest := filepath.Join(t.TempDir(), "bin")
	r := runInstall(t, installCfg{badChecksum: true, dest: dest})
	if r.exit == 0 {
		t.Fatalf("a checksum mismatch installed the binary anyway\n%s", r.out)
	}
	mustContain(t, "the refusal", r.stderr,
		"refusing to install",
		"want:", "got:",
		strings.Repeat("0", 64),
		"https://github.com/nuzur/nuzur-cli/releases/download/v1.6.1/nuzur-cli_Linux_x86_64.tar.gz",
		"https://github.com/nuzur/nuzur-cli/releases/download/v1.6.1/nuzur-cli_1.6.1_checksums.txt",
	)
	if got := dirEntries(t, dest); len(got) != 0 {
		t.Errorf("the destination is not empty after a refusal: %v", got)
	}
}

// A checksums file with no line for this asset is not "verified by default" — it
// is an unverifiable download, and it is the shape a release with a partial
// upload actually takes.
func TestInstallScriptMissingChecksumEntryFails(t *testing.T) {
	dest := filepath.Join(t.TempDir(), "bin")
	r := runInstall(t, installCfg{noChecksumEntry: true, dest: dest})
	if r.exit == 0 {
		t.Fatalf("a missing checksum entry installed the binary anyway\n%s", r.out)
	}
	mustContain(t, "the refusal", r.stderr,
		"no checksum for nuzur-cli_Linux_x86_64.tar.gz",
		"refusing to install",
		"https://github.com/nuzur/nuzur-cli/releases/download/v1.6.1/nuzur-cli_1.6.1_checksums.txt",
	)
	if got := dirEntries(t, dest); len(got) != 0 {
		t.Errorf("the destination is not empty after a refusal: %v", got)
	}
}

// The checksums file itself missing is the same class of problem: the archive is
// there but cannot be verified, so it is not installed.
func TestInstallScriptMissingChecksumsFileFails(t *testing.T) {
	dest := filepath.Join(t.TempDir(), "bin")
	r := runInstall(t, installCfg{noChecksums: true, dest: dest})
	if r.exit == 0 {
		t.Fatalf("a missing checksums file installed the binary anyway\n%s", r.out)
	}
	mustContain(t, "the refusal", r.stderr,
		"https://github.com/nuzur/nuzur-cli/releases/download/v1.6.1/nuzur-cli_1.6.1_checksums.txt",
		"nothing was installed",
	)
	if got := dirEntries(t, dest); len(got) != 0 {
		t.Errorf("the destination is not empty after a refusal: %v", got)
	}
}

// The failure the release window produces: a tag exists, its assets do not yet.
// The message names the file and says the three things that resolve it.
func TestInstallScriptDownloadFailureNamesTheURL(t *testing.T) {
	dest := filepath.Join(t.TempDir(), "bin")
	r := runInstall(t, installCfg{noAsset: true, dest: dest})
	if r.exit == 0 {
		t.Fatalf("a 404 on the asset still installed something\n%s", r.out)
	}
	mustContain(t, "the failure", r.stderr,
		"https://github.com/nuzur/nuzur-cli/releases/download/v1.6.1/nuzur-cli_Linux_x86_64.tar.gz",
		"still be uploading",
		"NUZUR_VERSION=",
		"https://nuzur.com/cli",
	)
}

// Both sha256 spellings, each on a PATH where the other does not exist.
//
// sha256sum is coreutils (Linux); shasum is the perl one macOS ships. Neither is
// universal, and the branch that is not taken on the developer's machine is
// exactly the one that breaks in CI — so each is tested with the other hidden.
// Across a macOS dev machine and the ubuntu CI runner, both branches run for
// real in the happy path too.
func TestInstallScriptSha256ToolFallback(t *testing.T) {
	for _, tool := range []string{"sha256sum", "shasum"} {
		t.Run("only "+tool, func(t *testing.T) {
			if _, err := exec.LookPath(tool); err != nil {
				t.Skipf("%s is not on this host", tool)
			}
			other := "shasum"
			if tool == "shasum" {
				other = "sha256sum"
			}
			dest := filepath.Join(t.TempDir(), "bin")
			r := runInstall(t, installCfg{dest: dest, utilities: []string{utilityDir(t, other)}})
			if r.exit != 0 {
				t.Fatalf("exit = %d with only %s available, want 0\n%s", r.exit, tool, r.out)
			}
			mustContain(t, "the run", r.out, "checksum verified", "installed")
		})
	}

	t.Run("neither refuses to install", func(t *testing.T) {
		dest := filepath.Join(t.TempDir(), "bin")
		r := runInstall(t, installCfg{dest: dest, utilities: []string{utilityDir(t, "sha256sum", "shasum")}})
		if r.exit == 0 {
			t.Fatalf("an unverifiable download was installed anyway\n%s", r.out)
		}
		mustContain(t, "the refusal", r.stderr, "sha256sum", "shasum", "refusing to install")
		if got := dirEntries(t, dest); len(got) != 0 {
			t.Errorf("the destination is not empty after a refusal: %v", got)
		}
	})
}

// ─────────────────────────────────────────────────────────────────────────────
// 7. Where it installs.
// ─────────────────────────────────────────────────────────────────────────────

// runChooseDest lifts nuzur_choose_dest (and the helpers it fails through) out of
// the script and runs it on its own. The block is delimited by markers in
// install.sh; this is the same block-extraction the bootstrap's apt guard uses,
// and for the same reason — the decision is a pure function of the environment,
// and running it directly is far cheaper than staging four whole installs.
func runChooseDest(t *testing.T, env []string) (string, string, int) {
	t.Helper()
	script := installScript(t)
	block := func(start, end string) string {
		i := strings.Index(script, start)
		j := strings.Index(script, end)
		if i < 0 || j < i {
			t.Fatalf("install.sh no longer carries the markers %q..%q — this test is asserting nothing", start, end)
		}
		return script[i:j]
	}
	body := "#!/bin/sh\nset -eu\n" +
		block(installHelpersStart, installHelpersEnd) +
		block(installDestStart, installDestEnd) +
		"\nnuzur_choose_dest\n"

	dir := t.TempDir()
	path := filepath.Join(dir, "dest.sh")
	if err := os.WriteFile(path, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("/bin/sh", path)
	cmd.Env = append([]string{"PATH=/usr/bin:/bin"}, env...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	exit := 0
	if err := cmd.Run(); err != nil {
		var ee *exec.ExitError
		if !errorsAs(err, &ee) {
			t.Fatalf("running the dest block: %v\n%s", err, stderr.String())
		}
		exit = ee.ExitCode()
	}
	return strings.TrimSpace(stdout.String()), stderr.String(), exit
}

// The order is the policy, and the policy is "never become root on your own".
func TestInstallScriptDestFallback(t *testing.T) {
	t.Run("1. NUZUR_INSTALL_DIR wins, even off PATH", func(t *testing.T) {
		explicit := filepath.Join(t.TempDir(), "explicit")
		home := t.TempDir()
		system := t.TempDir() // writable: would win if the explicit dir did not
		got, errOut, exit := runChooseDest(t, []string{
			"HOME=" + home,
			"NUZUR_INSTALL_DIR=" + explicit,
			"NUZUR_SYSTEM_BIN=" + system,
		})
		if exit != 0 {
			t.Fatalf("exit = %d\n%s", exit, errOut)
		}
		if got != explicit {
			t.Errorf("dest = %q, want the explicit %q", got, explicit)
		}
		if _, err := os.Stat(explicit); err != nil {
			t.Errorf("the explicit dir was not created: %v", err)
		}
	})

	t.Run("2. ~/.local/bin when it is already on PATH", func(t *testing.T) {
		home := t.TempDir()
		local := filepath.Join(home, ".local", "bin")
		system := t.TempDir() // writable, and still must not win
		got, errOut, exit := runChooseDest(t, []string{
			"HOME=" + home,
			"PATH=" + local + ":/usr/bin:/bin",
			"NUZUR_SYSTEM_BIN=" + system,
		})
		if exit != 0 {
			t.Fatalf("exit = %d\n%s", exit, errOut)
		}
		if got != local {
			t.Errorf("dest = %q, want %q — a per-user bin already on PATH needs no privileges and no advice", got, local)
		}
	})

	t.Run("3. the system bin when it is writable", func(t *testing.T) {
		home := t.TempDir()
		system := t.TempDir()
		got, errOut, exit := runChooseDest(t, []string{
			"HOME=" + home,
			"NUZUR_SYSTEM_BIN=" + system,
		})
		if exit != 0 {
			t.Fatalf("exit = %d\n%s", exit, errOut)
		}
		if got != system {
			t.Errorf("dest = %q, want the writable system bin %q", got, system)
		}
	})

	t.Run("4. ~/.local/bin, created, when nothing else works", func(t *testing.T) {
		home := t.TempDir()
		got, errOut, exit := runChooseDest(t, []string{
			"HOME=" + home,
			"NUZUR_SYSTEM_BIN=" + filepath.Join(t.TempDir(), "nope"),
		})
		if exit != 0 {
			t.Fatalf("exit = %d\n%s", exit, errOut)
		}
		want := filepath.Join(home, ".local", "bin")
		if got != want {
			t.Errorf("dest = %q, want %q", got, want)
		}
		if fi, err := os.Stat(want); err != nil || !fi.IsDir() {
			t.Errorf("%s was not created: %v", want, err)
		}
	})

	// The one case that is a refusal rather than a fallback: an explicit directory
	// the caller cannot write. Silently installing somewhere else would be worse
	// than failing — the caller asked for a specific path, probably a system one,
	// and the answer is the sudo re-run, not a different destination.
	t.Run("an unwritable explicit dir refuses and shows the sudo re-run", func(t *testing.T) {
		if os.Geteuid() == 0 {
			t.Skip("root can write anything")
		}
		locked := filepath.Join(t.TempDir(), "locked")
		if err := os.Mkdir(locked, 0o555); err != nil {
			t.Fatal(err)
		}
		_, errOut, exit := runChooseDest(t, []string{
			"HOME=" + t.TempDir(),
			"NUZUR_INSTALL_DIR=" + locked,
			"NUZUR_SYSTEM_BIN=" + t.TempDir(),
		})
		if exit == 0 {
			t.Fatal("an unwritable NUZUR_INSTALL_DIR was accepted")
		}
		mustContain(t, "the refusal", errOut, locked, "sudo NUZUR_INSTALL_DIR="+locked)
	})
}

// The dest is only useful if the shell can find it, and the fallback deliberately
// picks a directory that is often NOT on PATH. Saying so — with the exact export
// line — is the difference between a working install and "command not found".
func TestInstallScriptPathGuidanceWhenDestNotInPATH(t *testing.T) {
	t.Run("off PATH: says how to add it", func(t *testing.T) {
		dest := filepath.Join(t.TempDir(), "bin")
		r := runInstall(t, installCfg{dest: dest})
		if r.exit != 0 {
			t.Fatalf("exit = %d\n%s", r.exit, r.out)
		}
		mustContain(t, "the report", r.out, dest+" is not on your PATH", "export PATH=\""+dest+":$PATH\"")
	})

	t.Run("on PATH: stays quiet", func(t *testing.T) {
		dest := filepath.Join(t.TempDir(), "bin")
		if err := os.MkdirAll(dest, 0o755); err != nil {
			t.Fatal(err)
		}
		r := runInstall(t, installCfg{dest: dest, pathExtra: []string{dest}})
		if r.exit != 0 {
			t.Fatalf("exit = %d\n%s", r.exit, r.out)
		}
		if strings.Contains(r.out, "not on your PATH") {
			t.Errorf("the script gave PATH advice for a directory already on PATH:\n%s", r.out)
		}
	})
}

// A second nuzur-cli on PATH (brew, an old manual copy) is the one situation
// where a SUCCESSFUL install looks like a failed one: the shell either resolves
// the other binary outright, or keeps serving the old one from its command
// cache until `hash -r`. The first live user hit exactly this — installed
// 1.6.2, `nuzur-cli --version` said 1.6.1 (brew's) — so the report must name
// the coexisting install and the way out.
func TestInstallScriptWarnsAboutCoexistingInstalls(t *testing.T) {
	otherInstall := func(t *testing.T) string {
		t.Helper()
		dir := filepath.Join(t.TempDir(), "otherbin")
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		fake := "#!/bin/sh\necho \"Nuzur CLI version 0.0.9\"\n"
		if err := os.WriteFile(filepath.Join(dir, "nuzur-cli"), []byte(fake), 0o755); err != nil {
			t.Fatal(err)
		}
		return dir
	}

	t.Run("another install earlier on PATH: shadow warning with the brew remedy", func(t *testing.T) {
		dest := filepath.Join(t.TempDir(), "bin")
		if err := os.MkdirAll(dest, 0o755); err != nil {
			t.Fatal(err)
		}
		other := otherInstall(t)
		r := runInstall(t, installCfg{dest: dest, pathExtra: []string{other, dest}})
		if r.exit != 0 {
			t.Fatalf("exit = %d\n%s", r.exit, r.out)
		}
		mustContain(t, "the shadow warning", r.out,
			other+"/nuzur-cli", "comes FIRST on your PATH", "brew uninstall nuzur/tap/nuzur-cli")
	})

	t.Run("this install earlier on PATH: names the other copy and hash -r", func(t *testing.T) {
		dest := filepath.Join(t.TempDir(), "bin")
		if err := os.MkdirAll(dest, 0o755); err != nil {
			t.Fatal(err)
		}
		other := otherInstall(t)
		r := runInstall(t, installCfg{dest: dest, pathExtra: []string{dest, other}})
		if r.exit != 0 {
			t.Fatalf("exit = %d\n%s", r.exit, r.out)
		}
		mustContain(t, "the cache note", r.out,
			other+"/nuzur-cli", "comes first on PATH", "hash -r")
		if strings.Contains(r.out, "will shadow this install") {
			t.Errorf("claimed shadowing when this install resolves first:\n%s", r.out)
		}
	})

	t.Run("fresh install, no other copy: no coexistence noise", func(t *testing.T) {
		dest := filepath.Join(t.TempDir(), "bin")
		if err := os.MkdirAll(dest, 0o755); err != nil {
			t.Fatal(err)
		}
		r := runInstall(t, installCfg{dest: dest, pathExtra: []string{dest}})
		if r.exit != 0 {
			t.Fatalf("exit = %d\n%s", r.exit, r.out)
		}
		for _, absent := range []string{"another nuzur-cli", "hash -r"} {
			if strings.Contains(r.out, absent) {
				t.Errorf("a clean first install mentioned %q:\n%s", absent, r.out)
			}
		}
	})

	t.Run("upgrade in place, no other copy: hash -r note only", func(t *testing.T) {
		dest := filepath.Join(t.TempDir(), "bin")
		if err := os.MkdirAll(dest, 0o755); err != nil {
			t.Fatal(err)
		}
		cfg := installCfg{dest: dest, pathExtra: []string{dest}}
		if r := runInstall(t, cfg); r.exit != 0 {
			t.Fatalf("first install: exit = %d\n%s", r.exit, r.out)
		}
		r := runInstall(t, cfg)
		if r.exit != 0 {
			t.Fatalf("re-run: exit = %d\n%s", r.exit, r.out)
		}
		mustContain(t, "the upgrade cache note", r.out, "hash -r")
		if strings.Contains(r.out, "another nuzur-cli") {
			t.Errorf("invented a coexisting install on an in-place upgrade:\n%s", r.out)
		}
	})
}

// ─────────────────────────────────────────────────────────────────────────────
// 8. The alias.
// ─────────────────────────────────────────────────────────────────────────────

// `nuzur` is the name people type; `nuzur-cli` is the binary every doc, message
// and URL uses. Homebrew ships the symlink, Scoop and raw archives cannot — so
// this installer is the other place it comes from, and a user following the
// one-liner must end up with the same two names a brew user has.
func TestInstallScriptCreatesTheNuzurSymlink(t *testing.T) {
	t.Run("creates a relative symlink", func(t *testing.T) {
		dest := filepath.Join(t.TempDir(), "bin")
		r := runInstall(t, installCfg{dest: dest})
		if r.exit != 0 {
			t.Fatalf("exit = %d\n%s", r.exit, r.out)
		}
		link := filepath.Join(dest, "nuzur")
		target, err := os.Readlink(link)
		if err != nil {
			t.Fatalf("no nuzur symlink: %v", err)
		}
		// Relative, so the pair survives the directory being moved or mounted
		// elsewhere.
		if target != "nuzur-cli" {
			t.Errorf("symlink target = %q, want the relative %q", target, "nuzur-cli")
		}
		out, err := exec.Command(link, "--version").CombinedOutput()
		if err != nil {
			t.Fatalf("the alias does not run: %v\n%s", err, out)
		}
		if !strings.Contains(string(out), "1.6.1") {
			t.Errorf("the alias reports %q", out)
		}
		mustContain(t, "the report", r.out, "alias: nuzur")
	})

	// Re-running must not accumulate anything or fail on the existing link.
	t.Run("re-linking an existing symlink is fine", func(t *testing.T) {
		dest := filepath.Join(t.TempDir(), "bin")
		runInstall(t, installCfg{dest: dest})
		r := runInstall(t, installCfg{dest: dest})
		if r.exit != 0 {
			t.Fatalf("the second run failed: %d\n%s", r.exit, r.out)
		}
		if target, err := os.Readlink(filepath.Join(dest, "nuzur")); err != nil || target != "nuzur-cli" {
			t.Errorf("readlink = %q, %v", target, err)
		}
	})

	// A `nuzur` that is a real file belongs to somebody else — quite possibly
	// another tool's binary. Clobbering it would be this script deleting a program
	// it knows nothing about, so it warns and leaves it.
	t.Run("leaves a pre-existing regular file alone", func(t *testing.T) {
		dest := filepath.Join(t.TempDir(), "bin")
		if err := os.MkdirAll(dest, 0o755); err != nil {
			t.Fatal(err)
		}
		other := filepath.Join(dest, "nuzur")
		if err := os.WriteFile(other, []byte("#!/bin/sh\necho not ours\n"), 0o755); err != nil {
			t.Fatal(err)
		}
		r := runInstall(t, installCfg{dest: dest})
		if r.exit != 0 {
			t.Fatalf("exit = %d\n%s", r.exit, r.out)
		}
		mustContain(t, "the report", r.out, "already exists and is not a symlink", "use nuzur-cli")
		body, err := os.ReadFile(other)
		if err != nil || !strings.Contains(string(body), "not ours") {
			t.Errorf("the pre-existing file was replaced: %q, %v", body, err)
		}
		// ...and the real binary still landed.
		if _, err := os.Stat(filepath.Join(dest, "nuzur-cli")); err != nil {
			t.Errorf("nuzur-cli was not installed: %v", err)
		}
	})
}

// ─────────────────────────────────────────────────────────────────────────────
// 9. Running it twice.
// ─────────────────────────────────────────────────────────────────────────────

// The one-liner is what people paste to upgrade, so the second run is at least as
// common as the first. It has to be safe, and it has to SAY which of the two
// things happened — "installed" when there was nothing, the old and new versions
// when it moved, and plainly nothing-changed when it did not.
func TestInstallScriptUpgradeIsIdempotent(t *testing.T) {
	dest := filepath.Join(t.TempDir(), "bin")

	first := runInstall(t, installCfg{latestTag: "v1.5.2", dest: dest})
	if first.exit != 0 {
		t.Fatalf("first install failed: %d\n%s", first.exit, first.out)
	}
	mustContain(t, "the first run", first.out, "installed nuzur-cli version 1.5.2")
	if strings.Contains(first.out, "upgraded") {
		t.Errorf("a fresh install claimed an upgrade:\n%s", first.out)
	}

	up := runInstall(t, installCfg{latestTag: "v1.6.1", dest: dest})
	if up.exit != 0 {
		t.Fatalf("upgrade failed: %d\n%s", up.exit, up.out)
	}
	mustContain(t, "the upgrade", up.out, "upgraded nuzur-cli version 1.5.2 → nuzur-cli version 1.6.1")

	same := runInstall(t, installCfg{latestTag: "v1.6.1", dest: dest})
	if same.exit != 0 {
		t.Fatalf("re-run failed: %d\n%s", same.exit, same.out)
	}
	mustContain(t, "the re-run", same.out, "reinstalled nuzur-cli version 1.6.1", "already up to date")
	if strings.Contains(same.out, "upgraded") {
		t.Errorf("a same-version re-run claimed an upgrade:\n%s", same.out)
	}

	if got := dirEntries(t, dest); len(got) != 2 {
		t.Errorf("three runs left %v, want exactly nuzur-cli and nuzur", got)
	}
}
