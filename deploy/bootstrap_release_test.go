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
	want := CLIReleaseAssetURL(markerVersion, "${NUZUR_ARCH}")
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
	if want := CLIReleaseAssetURL(constants.CLI_VERSION, "${NUZUR_ARCH}"); !strings.Contains(pinned, want) {
		t.Errorf("a bootstrap rendered with no CLIVersion does not download %s", want)
	}
}

// The `v` handling is the one piece of string surgery in the URL, and it is the
// difference between a working download and a 404 that looks like a missing
// release.
func TestCLIReleaseAssetURLNormalizesTheVersion(t *testing.T) {
	want := "https://github.com/nuzur/nuzur-cli/releases/download/v1.5.2/nuzur-cli_Linux_x86_64.tar.gz"
	for _, in := range []string{"1.5.2", "v1.5.2", " v1.5.2 "} {
		if got := CLIReleaseAssetURL(in, CLIReleaseArchX8664); got != want {
			t.Errorf("CLIReleaseAssetURL(%q) = %q, want %q", in, got, want)
		}
	}
}
