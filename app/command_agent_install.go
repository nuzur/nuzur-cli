package app

import (
	"fmt"
	"os"
	"runtime"

	"github.com/nuzur/nuzur-cli/agent"
	"github.com/nuzur/nuzur-cli/files"
	"github.com/urfave/cli"
)

func (i *Implementation) AgentInstallCommand() cli.Command {
	return cli.Command{
		Name:  "install",
		Usage: i.localize.Localize("agent_install_desc", "Install the local agent as an OS-managed service (auto-start at login). Currently supports macOS and Linux."),
		Action: func(c *cli.Context) error {
			res, err := agent.Install()
			if err != nil {
				return err
			}
			fmt.Printf("Installed nuzur agent (%s).\n  unit:   %s\n  manual reload: %s\n",
				res.Platform, res.UnitPath, res.LoadCmd)
			return nil
		},
	}
}

func (i *Implementation) AgentUninstallCommand() cli.Command {
	return cli.Command{
		Name:  "uninstall",
		Usage: i.localize.Localize("agent_uninstall_desc", "Stop the auto-start service and remove the installed unit file"),
		Action: func(c *cli.Context) error {
			if err := agent.Uninstall(); err != nil {
				return err
			}
			fmt.Println("Uninstalled nuzur agent service.")
			return nil
		},
	}
}

// AgentStatusCommand prints local pairing + service-install state. Doesn't
// hit the cloud — it's a cheap local-state check intended for "is my daemon
// configured correctly?" troubleshooting.
func (i *Implementation) AgentStatusCommand() cli.Command {
	return cli.Command{
		Name:  "status",
		Usage: i.localize.Localize("agent_status_desc", "Show local pairing and auto-start service state"),
		Action: func(c *cli.Context) error {
			// Pairing state
			fmt.Printf("Pairing\n")
			if uuid, err := readExistingPairingUUID(); err == nil && uuid != "" {
				fmt.Printf("  uuid:  %s\n", uuid)
				fmt.Printf("  creds: %s\n", files.LocalAgentUUIDFilePath())
			} else {
				fmt.Println("  not paired (run `nuzur-cli agent pair`)")
			}

			// DSN/driver state
			driver, _ := readTrimmedFile(files.LocalAgentDriverFilePath())
			dsn, _ := readTrimmedFile(files.LocalAgentDSNFilePath())
			fmt.Printf("\nDefault DB (fallback for queries without a registered connection)\n")
			if driver == "" && dsn == "" {
				fmt.Println("  not configured (will prompt on `nuzur-cli agent start`)")
			} else {
				fmt.Printf("  driver: %s\n", orPlaceholder(driver, "(unset)"))
				fmt.Printf("  dsn:    %s\n", maskDSN(dsn))
			}

			// Service install state
			fmt.Printf("\nAuto-start service\n  platform: %s\n", runtime.GOOS)
			path, ok := serviceUnitPath()
			if !ok {
				fmt.Println("  status:   unsupported platform (no install available)")
			} else if _, err := os.Stat(path); err == nil {
				fmt.Printf("  status:   installed\n  unit:     %s\n", path)
			} else {
				fmt.Printf("  status:   not installed (run `nuzur-cli agent install`)\n  expected: %s\n", path)
			}

			return nil
		},
	}
}

// serviceUnitPath returns the platform service-file path that `nuzur-cli agent
// install` would create, or ("", false) on unsupported platforms.
func serviceUnitPath() (string, bool) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", false
	}
	switch runtime.GOOS {
	case "darwin":
		return home + "/Library/LaunchAgents/com.nuzur.agent.plist", true
	case "linux":
		return home + "/.config/systemd/user/nuzur-agent.service", true
	}
	return "", false
}

func orPlaceholder(s, placeholder string) string {
	if s == "" {
		return placeholder
	}
	return s
}
