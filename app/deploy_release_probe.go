package app

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/nuzur/nuzur-cli/constants"
	"github.com/nuzur/nuzur-cli/deploy"
	"github.com/nuzur/nuzur-cli/outputtools"
)

// deploy_release_probe.go answers one question before a deploy spends anything:
// does the nuzur-cli release the box is about to download actually exist?
//
// The failure it retires is the most expensive shape in the pipeline. The box
// installs the SAME CLI version as the one driving the deploy (see
// BootstrapParams.CLIVersion), and it installs it in section 6 of the bootstrap —
// after the VM has been created, Docker installed, the database provisioned and
// the application image built. A dev build, or a release whose assets are still
// uploading, therefore fails at the very end of the expensive part, and the user
// pays for a server that never finished. One HTTP request up front turns that
// into a refusal that has provisioned nothing.
//
// It is a CHECK, not a gate: only a definitive "that file is not there" stops the
// deploy. Anything else — a timeout, a proxy, DNS, GitHub returning 500 or rate
// limiting us — is this machine failing to reach GitHub, which says nothing about
// whether the BOX can. Those warn and continue, the same one-directional
// best-effort rule the pre-flight schema gate follows: a check may block on what
// it knows, never on what it could not find out.

// cliReleaseProbeTimeout bounds the whole request. The deploy is about to take
// minutes; five seconds is enough for a HEAD to GitHub and short enough that a
// hung proxy costs nothing worth mentioning.
const cliReleaseProbeTimeout = 5 * time.Second

// httpClient is the client the release probe uses.
//
// nil httpTransport — which is every production build — yields the real client
// with the real transport. A test sets the field to script GitHub's answer.
func (i *Implementation) httpClient() *http.Client {
	return &http.Client{
		Timeout:   cliReleaseProbeTimeout,
		Transport: i.httpTransport, // nil ⇒ http.DefaultTransport
	}
}

// checkCLIReleaseAsset is the deploy's use of the probe: the skip, the refusal
// and the warning in one place, so runDeploy carries three lines and the policy
// stays testable.
//
// --cli-install-cmd skips it ENTIRELY. That flag replaces the download with the
// user's own command, so the box never asks GitHub for anything and a probe of a
// URL nothing will fetch could only ever produce a false refusal — of the very
// escape hatch the refusal recommends.
func (i *Implementation) checkCLIReleaseAsset(s *deploySettings) error {
	if strings.TrimSpace(s.CLIInstallCmd) != "" {
		return nil
	}
	block, warn := probeCLIReleaseAsset(i.httpClient(), constants.CLI_VERSION)
	if block != nil {
		return block
	}
	if warn != "" {
		outputtools.PrintlnColoredErr(warn, outputtools.Yellow)
	}
	return nil
}

// probeCLIReleaseAsset asks whether the release asset the bootstrap will download
// exists.
//
// Returns (block, warn), at most one of which is non-empty:
//
//   - block: GitHub answered definitively that the file is not there (404/410).
//     The bootstrap WILL fail, so the deploy stops before provisioning.
//   - warn: the check itself did not complete, or GitHub answered something that
//     is about GitHub rather than about the asset. The deploy continues.
//
// It probes the Linux x86_64 asset specifically. That is what every managed
// provider hands out by default, and — more to the point — a version with no
// published release has no assets AT ALL, so this one 404 detects the entire
// failure it exists for. The gap is narrow and worth naming: an arm64 box whose
// release published x86_64 but not arm64 passes this probe and still fails on the
// box, with the bootstrap's own message.
func probeCLIReleaseAsset(client *http.Client, version string) (block error, warn string) {
	url := deploy.CLIReleaseAssetURL(version, deploy.CLIReleaseArchX8664)

	ctx, cancel := context.WithTimeout(context.Background(), cliReleaseProbeTimeout)
	defer cancel()
	// HEAD: the asset is a tarball and this wants its existence, not its bytes.
	req, err := http.NewRequestWithContext(ctx, http.MethodHead, url, nil)
	if err != nil {
		return nil, cliReleaseProbeWarning(url, err.Error())
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, cliReleaseProbeWarning(url, probeFailureCause(err))
	}
	defer resp.Body.Close()

	switch resp.StatusCode {
	case http.StatusNotFound, http.StatusGone:
		return cliReleaseMissingError(url, version, resp.StatusCode), ""
	}
	if resp.StatusCode >= 400 {
		// 403 (rate limit), 5xx, anything else: GitHub is unhappy with US, which
		// is not evidence about the file.
		return nil, cliReleaseProbeWarning(url, fmt.Sprintf("HTTP %d", resp.StatusCode))
	}
	return nil, ""
}

// cliReleaseMissingError is the refusal. It names the URL, because that is the
// one fact that lets a user check the claim themselves, and it names
// --cli-install-cmd, because that is the way forward for every case in which the
// user is right and the probe is inconvenient.
func cliReleaseMissingError(url, version string, status int) error {
	return fmt.Errorf(
		"nuzur-cli v%s has no published Linux release asset (%s returned %d), and the box installs the SAME version as the CLI running this deploy.\n"+
			"The bootstrap downloads that file in its last section — after the server, Docker, the database and the application image have all been created and paid for — so this deploy would fail there rather than here.\n"+
			"Either deploy from a released CLI version, or re-run with --cli-install-cmd '<command that installs nuzur-cli on the box>' to install it another way (that flag skips this check entirely).",
		version, url, status)
}

// probeFailureCause is the reason a request failed, without net/http's *url.Error
// wrapper. That wrapper repeats the method and the full URL, which the warning
// already states — printing both makes a one-line notice about something that
// does not matter read like the loudest message in the transcript.
func probeFailureCause(err error) string {
	var uerr *url.Error
	if errors.As(err, &uerr) && uerr.Err != nil {
		return uerr.Err.Error()
	}
	return err.Error()
}

// cliReleaseProbeWarning is what the deploy says when the check could not be
// made. It states what was not established rather than implying a problem with
// the release, and says where the real answer will come from if there is one.
func cliReleaseProbeWarning(url, cause string) string {
	return fmt.Sprintf(
		"warning: could not check that the nuzur-cli release the box installs is published (%s: %s) — continuing. "+
			"If it is missing, the bootstrap will say so when it tries the download.",
		url, cause)
}
