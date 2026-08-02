package app

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"

	"github.com/nuzur/nuzur-cli/constants"
	"github.com/nuzur/nuzur-cli/deploy"
)

// The probe's whole design is the split between "the file is not there" and "I
// could not find out", so that is what the table asserts: which statuses are
// allowed to stop a deploy, and that everything else lets it through.
func TestProbeCLIReleaseAsset(t *testing.T) {
	for _, tc := range []struct {
		name      string
		status    int
		transport error
		wantBlock bool
		wantWarn  bool
	}{
		{name: "a published release", status: 200},
		{name: "a redirect the client follows to the asset", status: 200},
		{
			// The case the probe exists for: a dev build, or a release whose
			// assets are still uploading.
			name: "no such asset", status: 404, wantBlock: true,
		},
		{name: "the asset was removed", status: 410, wantBlock: true},
		{
			// GitHub rate limiting is about US, not about the file. Blocking here
			// would refuse a perfectly deployable release.
			name: "rate limited", status: 403, wantWarn: true,
		},
		{name: "github is down", status: 503, wantWarn: true},
		{name: "an unrecognised server error", status: 500, wantWarn: true},
		{name: "the request timed out", transport: context.DeadlineExceeded, wantWarn: true},
		{name: "no route to the host", transport: errors.New("dial tcp: lookup github.com: no such host"), wantWarn: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rt := newFakeRoundTripper()
			rt.Status, rt.Err = tc.status, tc.transport
			client := &http.Client{Transport: rt}

			block, warn := probeCLIReleaseAsset(client, "1.2.3")

			if (block != nil) != tc.wantBlock {
				t.Errorf("block = %v, want blocking = %v", block, tc.wantBlock)
			}
			if (warn != "") != tc.wantWarn {
				t.Errorf("warn = %q, want warning = %v", warn, tc.wantWarn)
			}
			if block != nil && warn != "" {
				t.Errorf("the probe both blocked and warned: %v / %q", block, warn)
			}

			// Whatever it decided, it asked about the URL the bootstrap uses.
			want := "HEAD " + deploy.CLIReleaseAssetURL("1.2.3", deploy.CLIReleaseArchX8664)
			if reqs := rt.Requests(); len(reqs) != 1 || reqs[0] != want {
				t.Fatalf("requests = %v, want exactly [%q]", reqs, want)
			}

			// A refusal that does not say which file, and how to get past it, is
			// a dead end — see the message's own doc comment.
			if block != nil {
				for _, must := range []string{
					deploy.CLIReleaseAssetURL("1.2.3", deploy.CLIReleaseArchX8664),
					"--cli-install-cmd",
				} {
					if !strings.Contains(block.Error(), must) {
						t.Errorf("the block message does not mention %q:\n%s", must, block)
					}
				}
			}
		})
	}
}

// The block message is the only place the CLI states this failure BEFORE the box
// does, so it has to name the version it is talking about — the whole point being
// that the box installs the same one as the CLI running the deploy.
func TestProbeBlockNamesTheVersion(t *testing.T) {
	rt := newFakeRoundTripper()
	rt.Status = http.StatusNotFound

	block, _ := probeCLIReleaseAsset(&http.Client{Transport: rt}, constants.CLI_VERSION)
	if block == nil {
		t.Fatal("a 404 did not block")
	}
	if !strings.Contains(block.Error(), constants.CLI_VERSION) {
		t.Errorf("the block message does not name the version %s:\n%s", constants.CLI_VERSION, block)
	}
}

// The seam's default has to be the real thing, or the shipped binary would make
// no request at all — and the probe would silently pass on every deploy.
func TestHTTPClientDefaultsToTheRealTransport(t *testing.T) {
	i := &Implementation{}
	c := i.httpClient()
	if c.Transport != nil {
		t.Errorf("httpClient().Transport = %v, want nil (http.DefaultTransport)", c.Transport)
	}
	if c.Timeout != cliReleaseProbeTimeout {
		t.Errorf("httpClient().Timeout = %s, want %s", c.Timeout, cliReleaseProbeTimeout)
	}

	rt := newFakeRoundTripper()
	if got := (&Implementation{httpTransport: rt}).httpClient().Transport; got != http.RoundTripper(rt) {
		t.Errorf("a scripted transport did not reach the client: %v", got)
	}
}

// The escape hatch the refusal recommends has to actually escape. --cli-install-cmd
// replaces the download entirely, so the probe must not run at all — a check that
// blocked the one way past itself would be a trap, and this is the only test of
// that branch.
func TestCheckCLIReleaseAssetSkippedByInstallCmd(t *testing.T) {
	rt := newFakeRoundTripper()
	rt.Status = http.StatusNotFound // would block if it were consulted
	i := &Implementation{httpTransport: rt}

	if err := i.checkCLIReleaseAsset(&deploySettings{CLIInstallCmd: "curl -fsSL https://example/install | sh"}); err != nil {
		t.Fatalf("--cli-install-cmd did not skip the probe: %v", err)
	}
	if reqs := rt.Requests(); len(reqs) != 0 {
		t.Errorf("the probe ran despite --cli-install-cmd: %v", reqs)
	}

	// And without it, the same 404 stops the deploy.
	if err := i.checkCLIReleaseAsset(&deploySettings{}); err == nil {
		t.Error("a 404 did not stop a deploy that installs from GitHub")
	}
}
