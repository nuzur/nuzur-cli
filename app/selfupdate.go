package app

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/nuzur/nuzur-cli/constants"
	"github.com/nuzur/nuzur-cli/deploy"
)

// selfupdate.go keeps a CLI current, and tells its user when it is not.
//
// The problem it solves is not hypothetical. Much of what the CLI does is
// resolved server-side at run time — the sql-gen extension that renders DDL is
// fetched at its latest published version on every deploy — so a stale binary
// silently mixes old client behavior with new server behavior, and the parts
// that ARE in the binary (the --plan drift check, flag defaults, bug fixes) just
// quietly do not exist. A user on a months-old CLI has no signal at all: nothing
// errors, features are simply absent, and the docs describe a tool they do not
// have. One production user was 26 releases behind without knowing it.
//
// Two halves, deliberately asymmetric:
//
//   - `nuzur-cli update` — explicit, does the work, may fail loudly.
//   - the deploy-time notice — best-effort, one line, never blocks and never
//     fails a deploy. A version check that can break a deploy is worse than no
//     version check.
//
// It never overwrites a binary a package manager owns. Replacing brew's or
// scoop's file behind their back leaves the manager's metadata lying about what
// is installed, and the next `brew upgrade` silently reverts the user. Those
// installs are told which command to run instead.

// CLILatestReleaseAPIURL is the GitHub API endpoint that resolves the latest
// published nuzur-cli release.
//
// The API rather than the `releases/latest/download/...` redirect, for the same
// reason install.sh gives: a GitHub Release exists from the moment it is created,
// seconds before goreleaser finishes uploading its assets, so the redirect can
// resolve to a release whose files are not there yet. Asking for tag_name and
// then fetching that tag's assets keeps the resolve and the download talking
// about the same release. Drift-locked against install.sh by
// TestInstallScriptAndSelfUpdateResolveTheSameRelease.
const CLILatestReleaseAPIURL = "https://api.github.com/repos/nuzur/nuzur-cli/releases/latest"

// latestReleaseTimeout bounds the version lookup. The update command can afford
// to wait; the deploy notice cannot, and uses its own shorter budget below.
const latestReleaseTimeout = 10 * time.Second

// updateNoticeTimeout bounds the version check a deploy makes. A deploy takes
// minutes and this is a nicety — two seconds of a slow network is the most it
// may cost, after which the deploy proceeds as if it had never asked.
const updateNoticeTimeout = 2 * time.Second

// latestReleaseTag returns the tag_name of the latest published release, bare
// (no leading v).
func latestReleaseTag(ctx context.Context, client *http.Client) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, CLILatestReleaseAPIURL, nil)
	if err != nil {
		return "", err
	}
	// GitHub serves the v3 shape by default but says to ask for it explicitly.
	req.Header.Set("Accept", "application/vnd.github+json")
	resp, err := client.Do(req)
	if err != nil {
		return "", fmt.Errorf("could not reach %s: %w", CLILatestReleaseAPIURL, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		// 403 here is nearly always the unauthenticated rate limit, which is a
		// property of the network this ran on rather than of the release.
		return "", fmt.Errorf("%s returned HTTP %d", CLILatestReleaseAPIURL, resp.StatusCode)
	}

	var payload struct {
		TagName string `json:"tag_name"`
	}
	// Bounded: a redirected or hijacked endpoint should not be able to make this
	// allocate without limit. The real document is a few KB.
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&payload); err != nil {
		return "", fmt.Errorf("could not read the release from %s: %w", CLILatestReleaseAPIURL, err)
	}
	tag := strings.TrimPrefix(strings.TrimSpace(payload.TagName), "v")
	if tag == "" {
		return "", fmt.Errorf("%s returned no tag_name", CLILatestReleaseAPIURL)
	}
	return tag, nil
}

// compareVersions orders two dotted numeric versions: -1 if a < b, 0 if equal,
// 1 if a > b.
//
// Numeric per segment, not lexical: "1.10.0" is newer than "1.9.0", which a
// string compare gets backwards — and the CLI has already shipped a 1.10-shaped
// range of minors. A missing segment reads as 0 so "1.8" and "1.8.0" are equal.
// Any suffix on a segment (a "-rc1", a "+dirty", a local build marker) is
// ignored for ordering and only breaks a tie by making the version unparseable,
// which callers treat as "cannot compare" rather than "out of date".
func compareVersions(a, b string) int {
	as := strings.Split(strings.TrimPrefix(strings.TrimSpace(a), "v"), ".")
	bs := strings.Split(strings.TrimPrefix(strings.TrimSpace(b), "v"), ".")
	for i := 0; i < max(len(as), len(bs)); i++ {
		av, bv := versionSegment(as, i), versionSegment(bs, i)
		if av != bv {
			if av < bv {
				return -1
			}
			return 1
		}
	}
	return 0
}

// versionSegment reads one numeric segment, tolerating a suffix ("3-rc1" → 3)
// and treating a missing or non-numeric one as 0.
func versionSegment(segments []string, i int) int {
	if i >= len(segments) {
		return 0
	}
	s := segments[i]
	end := 0
	for end < len(s) && s[end] >= '0' && s[end] <= '9' {
		end++
	}
	if end == 0 {
		return 0
	}
	n, err := strconv.Atoi(s[:end])
	if err != nil {
		return 0
	}
	return n
}

// installMethod is how this binary got onto the machine, which decides whether
// it may be replaced in place.
type installMethod int

const (
	// installStandalone came from install.sh, a hand-extracted archive or a go
	// build — nothing tracks it, so this file is ours to replace.
	installStandalone installMethod = iota
	// installHomebrew is owned by brew: replacing the file leaves brew's metadata
	// wrong and the next `brew upgrade` reverts us.
	installHomebrew
	// installScoop is the same situation on Windows.
	installScoop
)

// detectInstallMethod classifies an executable path.
//
// Path-shape based, because it must work without running the package manager: a
// user who installed with brew may not have brew on PATH in the shell that runs
// this, and shelling out to `brew list` on every deploy notice would be both slow
// and wrong. Both managers have a stable, documented layout — brew keeps every
// binary under a Cellar (linked into bin as a symlink, which is why callers
// resolve symlinks BEFORE calling this), scoop under apps/<name>/<version>.
func detectInstallMethod(execPath string) installMethod {
	// Backslashes are folded explicitly rather than with filepath.ToSlash, which
	// is a no-op off Windows — the scoop cases would then only be recognised on
	// the very OS the tests do not run on.
	p := strings.ReplaceAll(execPath, `\`, "/")
	lower := strings.ToLower(p)
	switch {
	case strings.Contains(p, "/Cellar/"), strings.Contains(lower, "/homebrew/"), strings.Contains(lower, "/linuxbrew/"):
		return installHomebrew
	case strings.Contains(lower, "/scoop/apps/"), strings.Contains(lower, "/scoop/shims/"):
		return installScoop
	default:
		return installStandalone
	}
}

// upgradeCommand is what a package-managed install runs instead of this one.
func (m installMethod) upgradeCommand() string {
	switch m {
	case installHomebrew:
		return "brew upgrade nuzur-cli"
	case installScoop:
		return "scoop update nuzur-cli"
	default:
		return ""
	}
}

func (m installMethod) name() string {
	switch m {
	case installHomebrew:
		return "Homebrew"
	case installScoop:
		return "Scoop"
	default:
		return "standalone"
	}
}

// releaseAssetName is the goreleaser archive for the machine this is running on.
//
// goreleaser renders the OS title-cased and the architecture in uname's spelling
// rather than Go's, so both need mapping — the same two-way translation
// install.sh does with `uname`, here from the constants the binary was built
// with. An unmapped pair returns "", which callers turn into a refusal naming the
// platform rather than a 404 on an invented URL.
func releaseAssetName(goos, goarch string) (osName, arch string) {
	switch goos {
	case "darwin":
		osName = deploy.CLIReleaseOSDarwin
	case "linux":
		osName = deploy.CLIReleaseOSLinux
	default:
		return "", ""
	}
	switch goarch {
	case "amd64":
		arch = deploy.CLIReleaseArchX8664
	case "arm64":
		arch = "arm64"
	case "386":
		arch = "i386"
	default:
		return "", ""
	}
	// Apple dropped 32-bit execution, so this pair has no published asset.
	if osName == deploy.CLIReleaseOSDarwin && arch == "i386" {
		return "", ""
	}
	return osName, arch
}

// downloadAndVerify fetches a release archive and its checksum manifest and
// returns the archive's bytes, having checked them against the published sha256.
//
// The verification is not optional and not a flag. This function's output is
// about to become the binary the user runs against their production databases;
// "the bytes I got are the bytes that were published" has to be answered by the
// publisher rather than by whatever is between us and GitHub. install.sh refuses
// to install without it for the same reason.
func downloadAndVerify(ctx context.Context, client *http.Client, version, osName, arch string) ([]byte, error) {
	assetURL := deploy.CLIReleaseAssetURL(version, osName, arch)
	sumsURL := deploy.CLIReleaseChecksumsURL(version)

	archive, err := httpGetBytes(ctx, client, assetURL)
	if err != nil {
		return nil, fmt.Errorf("could not download %s: %w", assetURL, err)
	}
	sums, err := httpGetBytes(ctx, client, sumsURL)
	if err != nil {
		return nil, fmt.Errorf("the archive downloaded but its checksums did not (%s: %w) — nothing was installed", sumsURL, err)
	}

	assetName := fmt.Sprintf("nuzur-cli_%s_%s.tar.gz", osName, arch)
	want := checksumFor(string(sums), assetName)
	if want == "" {
		return nil, fmt.Errorf("no checksum published for %s in %s — refusing to install", assetName, sumsURL)
	}
	sum := sha256.Sum256(archive)
	if got := hex.EncodeToString(sum[:]); got != want {
		return nil, fmt.Errorf("checksum mismatch for %s — refusing to install\n  want: %s\n  got:  %s\n  archive:   %s\n  checksums: %s",
			assetName, want, got, assetURL, sumsURL)
	}
	return archive, nil
}

// httpGetBytes reads a whole response body, capped so a wrong URL that answers
// with something enormous cannot exhaust memory. The CLI archive is a few MB;
// 256MB is far above any real release and far below trouble.
func httpGetBytes(ctx context.Context, client *http.Client, url string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	return io.ReadAll(io.LimitReader(resp.Body, 256<<20))
}

// checksumFor picks one file's sha256 out of goreleaser's manifest, whose lines
// are "<hex>  <filename>". Matched on the exact filename so
// nuzur-cli_Linux_arm64.tar.gz never satisfies a request for
// nuzur-cli_Linux_x86_64.tar.gz.
func checksumFor(manifest, assetName string) string {
	for line := range strings.SplitSeq(manifest, "\n") {
		fields := strings.Fields(strings.TrimSpace(line))
		if len(fields) == 2 && fields[1] == assetName {
			return fields[0]
		}
	}
	return ""
}

// extractBinary pulls the `nuzur-cli` member out of a release tarball. The
// archive also carries LICENSE and README, which have no business landing in a
// bin directory.
func extractBinary(archive []byte) ([]byte, error) {
	zr, err := gzip.NewReader(bytes.NewReader(archive))
	if err != nil {
		return nil, fmt.Errorf("release archive is not gzip: %w", err)
	}
	defer zr.Close()

	tr := tar.NewReader(zr)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			return nil, fmt.Errorf("release archive contains no nuzur-cli binary")
		}
		if err != nil {
			return nil, fmt.Errorf("could not read the release archive: %w", err)
		}
		// Name compared exactly rather than by suffix: a tar member called
		// ../../nuzur-cli must not match, and nothing else in this archive is
		// meant to be executed.
		if hdr.Name != "nuzur-cli" || hdr.Typeflag != tar.TypeReg {
			continue
		}
		return io.ReadAll(io.LimitReader(tr, 256<<20))
	}
}

// replaceExecutable writes the new binary over the current one.
//
// Staged in the DESTINATION directory, not a temp dir: os.Rename is only atomic
// within a filesystem, and /tmp is frequently a different one — a cross-device
// rename fails, and copying instead would leave a half-written binary if it
// failed midway. Writing the staged file beside the target keeps the swap atomic,
// so a user either has the old CLI or the new one and never a truncated file.
//
// The running binary's own inode stays open and valid on unix, which is what
// makes replacing yourself safe here (and what makes it fail on Windows, handled
// by the caller).
func replaceExecutable(path string, binary []byte) error {
	dir := filepath.Dir(path)
	staged, err := os.CreateTemp(dir, ".nuzur-cli-update-*")
	if err != nil {
		return fmt.Errorf("cannot write to %s: %w\nIf nuzur-cli is installed system-wide, re-run with sudo.", dir, err)
	}
	stagedPath := staged.Name()
	// Removed on every failure path below. A stray .nuzur-cli-update-* in a bin
	// directory is confusing at best.
	defer os.Remove(stagedPath)

	if _, err := staged.Write(binary); err != nil {
		staged.Close()
		return fmt.Errorf("could not write the new binary: %w", err)
	}
	if err := staged.Close(); err != nil {
		return fmt.Errorf("could not write the new binary: %w", err)
	}
	// Before the rename: the file has to be executable the instant it takes the
	// real name, or a concurrent invocation finds a binary it cannot run.
	if err := os.Chmod(stagedPath, 0o755); err != nil {
		return fmt.Errorf("could not make the new binary executable: %w", err)
	}
	if err := os.Rename(stagedPath, path); err != nil {
		return fmt.Errorf("could not replace %s: %w\nIf nuzur-cli is installed system-wide, re-run with sudo.", path, err)
	}
	return nil
}

// resolvedExecutablePath is the real path of the running binary, with symlinks
// resolved.
//
// Resolving matters twice: install.sh creates a `nuzur` symlink beside
// `nuzur-cli`, so an invocation through the alias must update the target rather
// than replace the link with a regular file; and brew links its Cellar binaries
// into bin the same way, which is how detectInstallMethod sees the Cellar path at
// all.
func resolvedExecutablePath() (string, error) {
	exe, err := os.Executable()
	if err != nil {
		return "", fmt.Errorf("could not locate the running nuzur-cli binary: %w", err)
	}
	resolved, err := filepath.EvalSymlinks(exe)
	if err != nil {
		// A path we cannot resolve is still worth reporting on; the caller only
		// writes to it after its own checks.
		return exe, nil
	}
	return resolved, nil
}

// updateNotice is the one line a deploy prints when the CLI is behind, or "" when
// it is current, could not be checked, or is a dev build.
//
// Everything about this is best-effort by construction: it takes a short-lived
// context, it returns a string rather than an error, and every failure mode
// returns "". A user mid-deploy does not need to hear that GitHub rate-limited a
// courtesy check.
func updateNotice(ctx context.Context, client *http.Client, current string) string {
	latest, err := latestReleaseTag(ctx, client)
	if err != nil {
		return ""
	}
	if compareVersions(current, latest) >= 0 {
		return ""
	}
	return fmt.Sprintf(
		"nuzur-cli %s is available (you have %s) — run `nuzur-cli update`. "+
			"Newer CLIs carry fixes and flags this one does not have; deploying from a stale CLI is a common source of surprises.",
		latest, current)
}

// noticeIfCLIOutdated prints the deploy-time update notice. Silent on every
// failure — see updateNotice.
func (i *Implementation) noticeIfCLIOutdated() string {
	ctx, cancel := context.WithTimeout(context.Background(), updateNoticeTimeout)
	defer cancel()

	client := &http.Client{Timeout: updateNoticeTimeout, Transport: i.httpTransport}
	return updateNotice(ctx, client, constants.CLI_VERSION)
}

// currentPlatformAsset resolves this machine's release asset names, or an error
// naming the platform when there is none to download.
func currentPlatformAsset() (osName, arch string, err error) {
	osName, arch = releaseAssetName(runtime.GOOS, runtime.GOARCH)
	if osName == "" || arch == "" {
		return "", "", fmt.Errorf("no published nuzur-cli release for %s/%s — see https://nuzur.com/cli for the install options on this platform",
			runtime.GOOS, runtime.GOARCH)
	}
	return osName, arch, nil
}
