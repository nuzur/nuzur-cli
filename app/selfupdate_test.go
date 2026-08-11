package app

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/nuzur/nuzur-cli/deploy"
)

// routedRoundTripper answers each request from a table keyed by URL substring, so
// one test can script the release API, the archive and the checksum manifest —
// the three separate fetches an update makes — without a server.
type routedRoundTripper struct {
	routes   map[string]routedResponse
	requests []string
	err      error
}

type routedResponse struct {
	status int
	body   []byte
}

func (r *routedRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	r.requests = append(r.requests, req.Method+" "+req.URL.String())
	if r.err != nil {
		return nil, r.err
	}
	for match, resp := range r.routes {
		if !strings.Contains(req.URL.String(), match) {
			continue
		}
		status := resp.status
		if status == 0 {
			status = http.StatusOK
		}
		return &http.Response{
			StatusCode: status,
			Header:     http.Header{},
			Body:       io.NopCloser(bytes.NewReader(resp.body)),
			Request:    req,
		}, nil
	}
	return &http.Response{
		StatusCode: http.StatusNotFound,
		Header:     http.Header{},
		Body:       io.NopCloser(strings.NewReader("")),
		Request:    req,
	}, nil
}

func newRoutedClient(rt *routedRoundTripper) *http.Client { return &http.Client{Transport: rt} }

// tarGzWith builds a release-shaped archive: the binary plus the LICENSE and
// README that really are in there, so the extractor is tested against the layout
// it will actually meet rather than a single-member ideal.
func tarGzWith(t *testing.T, binary []byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	tw := tar.NewWriter(zw)

	members := []struct {
		name string
		body []byte
	}{
		{"LICENSE", []byte("MIT")},
		{"nuzur-cli", binary},
		{"README.md", []byte("# nuzur-cli")},
	}
	for _, m := range members {
		if err := tw.WriteHeader(&tar.Header{
			Name: m.name, Mode: 0o755, Size: int64(len(m.body)), Typeflag: tar.TypeReg,
		}); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write(m.body); err != nil {
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

func sha256Hex(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// TestCompareVersions: the ordering has to be NUMERIC per segment. A string
// compare puts 1.10.0 before 1.9.0, which would tell a user on the newest
// release that they are behind — and, worse, tell one who really is behind that
// they are current.
func TestCompareVersions(t *testing.T) {
	for _, tc := range []struct {
		a, b string
		want int
	}{
		{"1.2.8", "1.8.3", -1},
		{"1.8.3", "1.2.8", 1},
		{"1.8.3", "1.8.3", 0},
		{"v1.8.3", "1.8.3", 0},
		{"1.8.3", "v1.8.3", 0},
		{"1.9.0", "1.10.0", -1},
		{"1.10.0", "1.9.0", 1},
		{"1.8", "1.8.0", 0},
		{"1.8.0", "1.8", 0},
		{"2.0.0", "1.99.99", 1},
		{"1.8.3", "1.8.4", -1},
		{" 1.8.3 ", "1.8.3", 0},
		{"1.8.3-rc1", "1.8.3", 0},
	} {
		t.Run(tc.a+" vs "+tc.b, func(t *testing.T) {
			if got := compareVersions(tc.a, tc.b); got != tc.want {
				t.Errorf("compareVersions(%q, %q) = %d, want %d", tc.a, tc.b, got, tc.want)
			}
		})
	}
}

// TestDetectInstallMethod: replacing a package manager's binary leaves its
// metadata wrong and lets the next `brew upgrade` revert the user, so the
// classification is what keeps the updater out of directories it does not own.
func TestDetectInstallMethod(t *testing.T) {
	for _, tc := range []struct {
		name string
		path string
		want installMethod
	}{
		{"homebrew on apple silicon", "/opt/homebrew/Cellar/nuzur-cli/1.8.2/bin/nuzur-cli", installHomebrew},
		{"homebrew on intel macs", "/usr/local/Cellar/nuzur-cli/1.8.2/bin/nuzur-cli", installHomebrew},
		{"homebrew prefix without a cellar segment", "/opt/homebrew/bin/nuzur-cli", installHomebrew},
		{"linuxbrew", "/home/linuxbrew/.linuxbrew/bin/nuzur-cli", installHomebrew},
		{"scoop app dir", `C:\Users\dev\scoop\apps\nuzur-cli\1.8.2\nuzur-cli.exe`, installScoop},
		{"scoop shim", `C:\Users\dev\scoop\shims\nuzur-cli.exe`, installScoop},
		{"install.sh system-wide", "/usr/local/bin/nuzur-cli", installStandalone},
		{"install.sh per-user", "/home/dev/.local/bin/nuzur-cli", installStandalone},
		{"a go build in a work tree", "/Users/dev/code/nuzur-cli/nuzur-cli", installStandalone},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := detectInstallMethod(tc.path); got != tc.want {
				t.Errorf("detectInstallMethod(%q) = %v, want %v", tc.path, got, tc.want)
			}
			// A managed install is only useful if it can say what to run instead.
			if tc.want != installStandalone && detectInstallMethod(tc.path).upgradeCommand() == "" {
				t.Error("a package-managed install must name its upgrade command")
			}
		})
	}
}

// TestReleaseAssetName maps Go's platform spellings onto goreleaser's. They
// disagree on every value, and an unmapped pair must refuse rather than compose a
// URL that 404s.
func TestReleaseAssetName(t *testing.T) {
	for _, tc := range []struct {
		goos, goarch   string
		wantOS, wantAr string
	}{
		{"darwin", "arm64", "Darwin", "arm64"},
		{"darwin", "amd64", "Darwin", "x86_64"},
		{"linux", "amd64", "Linux", "x86_64"},
		{"linux", "arm64", "Linux", "arm64"},
		{"linux", "386", "Linux", "i386"},
		// Apple dropped 32-bit execution; there is no such asset.
		{"darwin", "386", "", ""},
		{"windows", "amd64", "", ""},
		{"freebsd", "amd64", "", ""},
		{"linux", "riscv64", "", ""},
	} {
		t.Run(tc.goos+"/"+tc.goarch, func(t *testing.T) {
			osName, arch := releaseAssetName(tc.goos, tc.goarch)
			if osName != tc.wantOS || arch != tc.wantAr {
				t.Errorf("releaseAssetName(%q, %q) = (%q, %q), want (%q, %q)",
					tc.goos, tc.goarch, osName, arch, tc.wantOS, tc.wantAr)
			}
		})
	}
}

func TestLatestReleaseTag(t *testing.T) {
	t.Run("reads tag_name and strips the v", func(t *testing.T) {
		rt := &routedRoundTripper{routes: map[string]routedResponse{
			"releases/latest": {body: []byte(`{"tag_name":"v1.8.3","name":"1.8.3"}`)},
		}}
		got, err := latestReleaseTag(context.Background(), newRoutedClient(rt))
		if err != nil {
			t.Fatal(err)
		}
		if got != "1.8.3" {
			t.Errorf("latestReleaseTag = %q, want 1.8.3", got)
		}
	})

	t.Run("rate limiting is an error, not a version", func(t *testing.T) {
		rt := &routedRoundTripper{routes: map[string]routedResponse{
			"releases/latest": {status: http.StatusForbidden, body: []byte(`{"message":"API rate limit exceeded"}`)},
		}}
		if _, err := latestReleaseTag(context.Background(), newRoutedClient(rt)); err == nil {
			t.Fatal("expected an error on HTTP 403")
		}
	})

	t.Run("a document with no tag_name is an error", func(t *testing.T) {
		rt := &routedRoundTripper{routes: map[string]routedResponse{
			"releases/latest": {body: []byte(`{}`)},
		}}
		if _, err := latestReleaseTag(context.Background(), newRoutedClient(rt)); err == nil {
			t.Fatal("expected an error when tag_name is absent")
		}
	})

	t.Run("an unreachable network is an error", func(t *testing.T) {
		rt := &routedRoundTripper{err: errors.New("dial tcp: lookup api.github.com: no such host")}
		if _, err := latestReleaseTag(context.Background(), newRoutedClient(rt)); err == nil {
			t.Fatal("expected an error when the request fails")
		}
	})
}

// TestChecksumFor: the manifest lists every platform's archive, so the lookup
// has to match the filename EXACTLY. A prefix or suffix match would happily
// verify an arm64 download against the x86_64 checksum and reject a good file.
func TestChecksumFor(t *testing.T) {
	manifest := strings.Join([]string{
		"aaaa  nuzur-cli_Linux_arm64.tar.gz",
		"bbbb  nuzur-cli_Linux_x86_64.tar.gz",
		"cccc  nuzur-cli_Darwin_arm64.tar.gz",
	}, "\n")

	if got := checksumFor(manifest, "nuzur-cli_Linux_x86_64.tar.gz"); got != "bbbb" {
		t.Errorf("checksumFor = %q, want bbbb", got)
	}
	if got := checksumFor(manifest, "nuzur-cli_Darwin_arm64.tar.gz"); got != "cccc" {
		t.Errorf("checksumFor = %q, want cccc", got)
	}
	if got := checksumFor(manifest, "nuzur-cli_Windows_x86_64.tar.gz"); got != "" {
		t.Errorf("checksumFor for an absent asset = %q, want empty", got)
	}
}

func TestExtractBinary(t *testing.T) {
	want := []byte("#!/bin/sh\necho nuzur\n")
	got, err := extractBinary(tarGzWith(t, want))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, want) {
		t.Errorf("extractBinary = %q, want %q", got, want)
	}

	if _, err := extractBinary([]byte("not a gzip stream")); err == nil {
		t.Error("expected an error on a non-gzip archive")
	}
}

// TestDownloadAndVerify is the security-relevant one: this function's output
// becomes the binary the user runs against production databases, so a checksum
// that does not match has to stop the update rather than warn about it.
func TestDownloadAndVerify(t *testing.T) {
	archive := tarGzWith(t, []byte("binary"))
	assetName := fmt.Sprintf("nuzur-cli_%s_%s.tar.gz", deploy.CLIReleaseOSLinux, deploy.CLIReleaseArchX8664)

	t.Run("a matching checksum passes", func(t *testing.T) {
		rt := &routedRoundTripper{routes: map[string]routedResponse{
			"checksums.txt": {body: []byte(sha256Hex(archive) + "  " + assetName + "\n")},
			".tar.gz":       {body: archive},
		}}
		got, err := downloadAndVerify(context.Background(), newRoutedClient(rt), "1.8.3",
			deploy.CLIReleaseOSLinux, deploy.CLIReleaseArchX8664)
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(got, archive) {
			t.Error("returned bytes are not the downloaded archive")
		}
	})

	t.Run("a tampered archive is refused", func(t *testing.T) {
		rt := &routedRoundTripper{routes: map[string]routedResponse{
			"checksums.txt": {body: []byte(sha256Hex([]byte("something else")) + "  " + assetName + "\n")},
			".tar.gz":       {body: archive},
		}}
		_, err := downloadAndVerify(context.Background(), newRoutedClient(rt), "1.8.3",
			deploy.CLIReleaseOSLinux, deploy.CLIReleaseArchX8664)
		if err == nil {
			t.Fatal("expected a refusal on a checksum mismatch")
		}
		if !strings.Contains(err.Error(), "checksum mismatch") {
			t.Errorf("error should name the mismatch, got %v", err)
		}
	})

	t.Run("no published checksum is refused", func(t *testing.T) {
		rt := &routedRoundTripper{routes: map[string]routedResponse{
			"checksums.txt": {body: []byte("dddd  nuzur-cli_Darwin_arm64.tar.gz\n")},
			".tar.gz":       {body: archive},
		}}
		_, err := downloadAndVerify(context.Background(), newRoutedClient(rt), "1.8.3",
			deploy.CLIReleaseOSLinux, deploy.CLIReleaseArchX8664)
		if err == nil {
			t.Fatal("expected a refusal when the asset has no published checksum")
		}
	})

	t.Run("an archive that cannot be verified is not installed", func(t *testing.T) {
		rt := &routedRoundTripper{routes: map[string]routedResponse{
			"checksums.txt": {status: http.StatusNotFound},
			".tar.gz":       {body: archive},
		}}
		if _, err := downloadAndVerify(context.Background(), newRoutedClient(rt), "1.8.3",
			deploy.CLIReleaseOSLinux, deploy.CLIReleaseArchX8664); err == nil {
			t.Fatal("expected a refusal when the checksums cannot be fetched")
		}
	})
}

func TestReplaceExecutable(t *testing.T) {
	t.Run("swaps the binary and leaves it executable", func(t *testing.T) {
		dir := t.TempDir()
		path := filepath.Join(dir, "nuzur-cli")
		if err := os.WriteFile(path, []byte("old"), 0o755); err != nil {
			t.Fatal(err)
		}

		if err := replaceExecutable(path, []byte("new")); err != nil {
			t.Fatal(err)
		}

		got, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if string(got) != "new" {
			t.Errorf("binary = %q, want new", got)
		}
		info, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		if info.Mode().Perm()&0o111 == 0 {
			t.Errorf("replaced binary is not executable (mode %v)", info.Mode())
		}

		// Nothing staged is left behind: a stray .nuzur-cli-update-* in a bin
		// directory is confusing at best.
		entries, err := os.ReadDir(dir)
		if err != nil {
			t.Fatal(err)
		}
		for _, e := range entries {
			if strings.HasPrefix(e.Name(), ".nuzur-cli-update-") {
				t.Errorf("left a staged file behind: %s", e.Name())
			}
		}
	})

	t.Run("a directory it cannot write is an error, not a partial write", func(t *testing.T) {
		dir := t.TempDir()
		path := filepath.Join(dir, "nuzur-cli")
		if err := os.WriteFile(path, []byte("old"), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(dir, 0o500); err != nil {
			t.Skipf("cannot make the directory read-only on this platform: %v", err)
		}
		t.Cleanup(func() { os.Chmod(dir, 0o700) })

		if os.Geteuid() == 0 {
			t.Skip("running as root, which can write a read-only directory")
		}
		if err := replaceExecutable(path, []byte("new")); err == nil {
			t.Fatal("expected an error writing into a read-only directory")
		}
		// The old binary is untouched — that is the point of staging + rename.
		got, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if string(got) != "old" {
			t.Errorf("a failed update changed the binary: %q", got)
		}
	})
}

// TestUpdateNotice: everything about the deploy-time notice is best-effort, so
// every way of not knowing has to produce silence rather than noise or an error.
func TestUpdateNotice(t *testing.T) {
	behind := &routedRoundTripper{routes: map[string]routedResponse{
		"releases/latest": {body: []byte(`{"tag_name":"v1.8.3"}`)},
	}}
	notice := updateNotice(context.Background(), newRoutedClient(behind), "1.2.8")
	if notice == "" {
		t.Fatal("expected a notice when the CLI is behind")
	}
	for _, want := range []string{"1.8.3", "1.2.8", "nuzur-cli update"} {
		if !strings.Contains(notice, want) {
			t.Errorf("notice should mention %q, got %q", want, notice)
		}
	}

	current := &routedRoundTripper{routes: map[string]routedResponse{
		"releases/latest": {body: []byte(`{"tag_name":"v1.8.3"}`)},
	}}
	if got := updateNotice(context.Background(), newRoutedClient(current), "1.8.3"); got != "" {
		t.Errorf("no notice expected when current, got %q", got)
	}

	// A dev build ahead of the last release must not be told to downgrade.
	ahead := &routedRoundTripper{routes: map[string]routedResponse{
		"releases/latest": {body: []byte(`{"tag_name":"v1.8.3"}`)},
	}}
	if got := updateNotice(context.Background(), newRoutedClient(ahead), "1.9.0"); got != "" {
		t.Errorf("no notice expected for a build ahead of the release, got %q", got)
	}

	// Offline, rate-limited, behind a proxy: silence. A deploy does not need to
	// hear that a courtesy check failed.
	offline := &routedRoundTripper{err: errors.New("no route to host")}
	if got := updateNotice(context.Background(), newRoutedClient(offline), "1.2.8"); got != "" {
		t.Errorf("no notice expected when the check fails, got %q", got)
	}

	limited := &routedRoundTripper{routes: map[string]routedResponse{
		"releases/latest": {status: http.StatusForbidden},
	}}
	if got := updateNotice(context.Background(), newRoutedClient(limited), "1.2.8"); got != "" {
		t.Errorf("no notice expected when rate limited, got %q", got)
	}
}

// TestInstallScriptAndSelfUpdateResolveTheSameRelease drift-locks the endpoint
// against install.sh. If the two resolve "latest" differently, `nuzur-cli update`
// and the documented one-liner can install different versions from the same
// machine at the same moment — and the notice would be reporting on a release the
// installer would not fetch.
func TestInstallScriptAndSelfUpdateResolveTheSameRelease(t *testing.T) {
	script, err := os.ReadFile("../install.sh")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(script), CLILatestReleaseAPIURL) {
		t.Errorf("install.sh does not resolve the latest release from %s — the installer and `nuzur-cli update` would disagree about what 'latest' means",
			CLILatestReleaseAPIURL)
	}
}
