package deploy

import (
	"strings"
	"testing"

	"github.com/nuzur/nuzur-cli/constants"
)

// The bootstrap composes the release URL in shell and the CLI's pre-flight probe
// composes it in Go. Nothing but this test makes the two the same string, and the
// cost of them drifting is asymmetric: the probe would report "the release exists"
// about a URL the box never fetches, and the deploy would then fail on the box
// after the VM, Docker, the database and the app image have all been paid for.
//
// The marker version is deliberately not a real one — the assertion is about the
// SHAPE of the URL, and a version that happens to exist would let a hard-coded
// literal pass.
func TestBootstrapTemplateUsesCLIReleaseAssetURL(t *testing.T) {
	const markerVersion = "9.9.9-drift-marker"

	script, err := RenderBootstrap(BootstrapParams{
		Identifier:   "shop",
		DBEngine:     DBMySQL,
		DBName:       "shop",
		DBUser:       "shop_app",
		RemoteSrcDir: "/opt/nuzur/shop/src",
		CLIVersion:   markerVersion,
	})
	if err != nil {
		t.Fatalf("RenderBootstrap: %v", err)
	}

	// The template resolves the architecture on the box, so the Go form is asked
	// for the same placeholder the shell expands.
	want := CLIReleaseAssetURL(markerVersion, CLIReleaseOSLinux, "${NUZUR_ARCH}")
	if !strings.Contains(script, want) {
		t.Errorf("the rendered bootstrap does not contain CLIReleaseAssetURL's output.\n"+
			"  want the script to download from: %s\n"+
			"The template and deploy.CLIReleaseAssetURL have drifted — the release probe in\n"+
			"app/deploy_release_probe.go now checks a URL the box does not use.", want)
	}

	// And the pin itself: an unset CLIVersion must render the CLI's own version,
	// which is the property the probe relies on to know WHICH release to ask about.
	pinned, err := RenderBootstrap(BootstrapParams{
		Identifier:   "shop",
		DBEngine:     DBMySQL,
		DBName:       "shop",
		DBUser:       "shop_app",
		RemoteSrcDir: "/opt/nuzur/shop/src",
	})
	if err != nil {
		t.Fatalf("RenderBootstrap (default version): %v", err)
	}
	if want := CLIReleaseAssetURL(constants.CLI_VERSION, CLIReleaseOSLinux, "${NUZUR_ARCH}"); !strings.Contains(pinned, want) {
		t.Errorf("a bootstrap rendered with no CLIVersion does not download %s", want)
	}

	// The checksums file the download is verified against is the SAME single
	// definition, and it is the harder of the two to get right: the version appears
	// twice, with the `v` in the tag segment and without it in goreleaser's
	// filename. The marker version proves the shape rather than a lucky literal.
	if want := CLIReleaseChecksumsURL(markerVersion); !strings.Contains(script, want) {
		t.Errorf("the rendered bootstrap does not contain CLIReleaseChecksumsURL's output.\n"+
			"  want the script to verify against: %s\n"+
			"The template and deploy.CLIReleaseChecksumsURL have drifted, which means the\n"+
			"box would download a checksums file the installer does not use — or none at all.", want)
	}
}

// The version normalisation for the checksums URL, which is the one place the
// same version has to be rendered two different ways in a single string.
func TestCLIReleaseChecksumsURLNormalizesTheVersion(t *testing.T) {
	want := "https://github.com/nuzur/nuzur-cli/releases/download/v1.6.1/nuzur-cli_1.6.1_checksums.txt"
	for _, in := range []string{"1.6.1", "v1.6.1", " v1.6.1 "} {
		if got := CLIReleaseChecksumsURL(in); got != want {
			t.Errorf("CLIReleaseChecksumsURL(%q) = %q, want %q", in, got, want)
		}
	}
}

// The verification has to happen BEFORE the archive is unpacked.
//
// Order is the entire safety property here: an archive that is not the published
// one must never be extracted, let alone installed to a system path and then run
// as root by the agent's systemd unit. Checking afterwards would verify a file
// whose contents are already on the box. So this asserts the index of the compare
// against the index of the tar — the only way to state "before" about a script.
func TestBootstrapVerifiesTheCLIChecksum(t *testing.T) {
	script, err := RenderBootstrap(BootstrapParams{
		Identifier:   "shop",
		DBEngine:     DBMySQL,
		DBName:       "shop",
		DBUser:       "shop_app",
		RemoteSrcDir: "/opt/nuzur/shop/src",
	})
	if err != nil {
		t.Fatalf("RenderBootstrap: %v", err)
	}

	for _, want := range []string{
		// the manifest, fetched with its own named-URL failure
		"NUZUR_CLI_SUMS_URL=",
		"could not download the checksums for nuzur-cli",
		// the entry for THIS box's architecture, not just any line in the file
		"nuzur-cli_Linux_${NUZUR_ARCH}.tar.gz",
		// coreutils sha256sum: guaranteed on the Ubuntu/Debian boxes this targets
		"sha256sum /tmp/nuzur-cli.tar.gz",
		// and the two refusals, which must say what they refuse to do
		"refusing to install",
		"checksum mismatch for nuzur-cli_Linux_${NUZUR_ARCH}.tar.gz",
	} {
		if !strings.Contains(script, want) {
			t.Errorf("the rendered bootstrap is missing %q", want)
		}
	}

	verify := strings.Index(script, "NUZUR_CLI_GOT_SUM")
	untar := strings.Index(script, "tar -xzf /tmp/nuzur-cli.tar.gz")
	install := strings.Index(script, "install -m 0755 /tmp/nuzur-cli ")
	if verify < 0 || untar < 0 || install < 0 {
		t.Fatalf("the CLI install section is not where this test expects it (verify=%d untar=%d install=%d)", verify, untar, install)
	}
	if verify > untar {
		t.Error("the bootstrap unpacks the nuzur-cli tarball BEFORE verifying its checksum — an unverified archive must never be extracted")
	}
	if untar > install {
		t.Error("the bootstrap installs before it extracts, which cannot be right")
	}
}

// The `v` handling is the one piece of string surgery in the URL, and it is the
// difference between a working download and a 404 that looks like a missing
// release.
func TestCLIReleaseAssetURLNormalizesTheVersion(t *testing.T) {
	want := "https://github.com/nuzur/nuzur-cli/releases/download/v1.5.2/nuzur-cli_Linux_x86_64.tar.gz"
	for _, in := range []string{"1.5.2", "v1.5.2", " v1.5.2 "} {
		if got := CLIReleaseAssetURL(in, CLIReleaseOSLinux, CLIReleaseArchX8664); got != want {
			t.Errorf("CLIReleaseAssetURL(%q) = %q, want %q", in, got, want)
		}
	}
}
