package app

import (
	"context"
	"fmt"
	"net/http"
	"runtime"

	"github.com/nuzur/nuzur-cli/constants"
	"github.com/nuzur/nuzur-cli/outputtools"
	"github.com/urfave/cli"
)

// UpdateCommand upgrades the CLI in place.
//
// It exists because "keep your CLI current" was advice with no command behind
// it: install.sh could upgrade, but only if you remembered the one-liner, and
// nothing ever told you that you were behind. See selfupdate.go for why a stale
// CLI is quietly harmful rather than merely old.
func (i *Implementation) UpdateCommand() cli.Command {
	return cli.Command{
		Name:  "update",
		Usage: i.localize.Localize("update_desc", "Update nuzur-cli to the latest released version"),
		Flags: []cli.Flag{
			cli.BoolFlag{
				Name:  "check",
				Usage: "Only report whether a newer version exists; install nothing. Flag-only.",
			},
		},
		Action: func(c *cli.Context) error {
			if err := requireNoArgs(c, "update"); err != nil {
				return err
			}
			return i.runUpdate(c.Bool("check"))
		},
	}
}

func (i *Implementation) runUpdate(checkOnly bool) error {
	client := &http.Client{Timeout: latestReleaseTimeout, Transport: i.httpTransport}
	ctx, cancel := context.WithTimeout(context.Background(), latestReleaseTimeout)
	defer cancel()

	latest, err := latestReleaseTag(ctx, client)
	if err != nil {
		return fmt.Errorf("could not check for a newer nuzur-cli: %w\nInstall a specific version instead:\n  curl -fsSL https://nuzur.com/install.sh | NUZUR_VERSION=v1.2.3 sh", err)
	}

	current := constants.CLI_VERSION
	switch {
	case compareVersions(current, latest) == 0:
		outputtools.PrintlnColored(fmt.Sprintf("nuzur-cli %s is up to date.", current), outputtools.Green)
		return nil
	case compareVersions(current, latest) > 0:
		// A local build ahead of the last release. Saying "up to date" would be a
		// lie and downgrading would be a surprise, so it reports and stops.
		outputtools.PrintlnColored(fmt.Sprintf(
			"nuzur-cli %s is NEWER than the latest published release (%s) — this looks like a development build. Nothing to update.",
			current, latest), outputtools.Yellow)
		return nil
	}

	outputtools.PrintlnColored(fmt.Sprintf("nuzur-cli %s is available (you have %s).", latest, current), outputtools.Blue)
	if checkOnly {
		outputtools.PrintlnColored("Run `nuzur-cli update` to install it.", outputtools.Blue)
		return nil
	}

	execPath, err := resolvedExecutablePath()
	if err != nil {
		return err
	}

	// A package manager owns this binary: tell the user its command rather than
	// replacing the file behind the manager's back, which would leave its
	// metadata wrong and let the next `brew upgrade` silently revert the user.
	if method := detectInstallMethod(execPath); method != installStandalone {
		outputtools.PrintlnColored(fmt.Sprintf(
			"This nuzur-cli was installed with %s (%s), which owns the binary — update it with:\n  %s",
			method.name(), execPath, method.upgradeCommand()), outputtools.Yellow)
		return nil
	}

	// Windows standalone installs cannot be replaced in place: the OS holds a
	// lock on a running executable, so the rename fails after the download. Say
	// so before spending it.
	if runtime.GOOS == "windows" {
		return fmt.Errorf("nuzur-cli cannot replace itself on Windows — the running binary is locked by the OS.\n"+
			"Update with Scoop:\n  scoop update nuzur-cli\n"+
			"or download v%s by hand from https://github.com/nuzur/nuzur-cli/releases", latest)
	}

	osName, arch, err := currentPlatformAsset()
	if err != nil {
		return err
	}

	outputtools.PrintlnColored(fmt.Sprintf("Downloading nuzur-cli %s for %s/%s…", latest, osName, arch), outputtools.Blue)
	archive, err := downloadAndVerify(ctx, client, latest, osName, arch)
	if err != nil {
		return err
	}
	outputtools.PrintlnColored("Checksum verified.", outputtools.Green)

	binary, err := extractBinary(archive)
	if err != nil {
		return err
	}
	if err := replaceExecutable(execPath, binary); err != nil {
		return err
	}

	outputtools.PrintlnColored(fmt.Sprintf("Updated nuzur-cli %s → %s (%s).", current, latest, execPath), outputtools.Green)
	// The same cache caveat install.sh prints: an already-open shell can keep
	// running the old binary from its command-location cache, which reads as the
	// update having failed.
	outputtools.PrintlnColored("Already-open shells may still run the old version from cache — open a new terminal, or run: hash -r", outputtools.Blue)
	return nil
}
