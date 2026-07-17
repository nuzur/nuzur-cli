// Package agent's install.go provides the platform-specific "auto-start at
// login" hooks. macOS uses launchd's per-user LaunchAgents; Linux uses
// systemd --user units. Windows is intentionally left out for now — Task
// Scheduler XML is verbose enough that it warrants a dedicated pass.
package agent

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
)

const (
	launchdLabel  = "com.nuzur.agent"
	systemdUnitID = "nuzur-agent"
	stdoutLogName = "agent.log"
	stderrLogName = "agent.err"
)

// agentLogDir returns the per-user directory for the auto-start service's
// stdout/err logs. Uses the persistent config dir so logs survive reboots and
// resolve correctly on Windows, falling back to the OS temp dir.
func agentLogDir() string {
	if d, err := os.UserConfigDir(); err == nil {
		return filepath.Join(d, "nuzur", "logs")
	}
	return filepath.Join(os.TempDir(), "nuzur-cli")
}

// InstallResult describes where the service file landed so the caller (the
// `nuzur-cli agent install` command) can print something useful.
type InstallResult struct {
	Platform string
	UnitPath string
	LoadCmd  string
}

// Install writes the platform-appropriate service definition and loads it so
// the daemon starts immediately and survives reboots.
func Install() (*InstallResult, error) {
	execPath, err := os.Executable()
	if err != nil {
		return nil, fmt.Errorf("could not resolve nuzur cli binary path: %w", err)
	}
	// Resolve symlinks so an upgrade replacing the symlink target doesn't
	// silently break the service definition.
	if real, err := filepath.EvalSymlinks(execPath); err == nil {
		execPath = real
	}
	if err := os.MkdirAll(agentLogDir(), 0o700); err != nil {
		return nil, err
	}

	switch runtime.GOOS {
	case "darwin":
		return installMacOS(execPath)
	case "linux":
		return installLinux(execPath)
	case "windows":
		return nil, fmt.Errorf("windows install is not implemented yet; run `nuzur-cli agent start` manually for now")
	default:
		return nil, fmt.Errorf("unsupported platform: %s", runtime.GOOS)
	}
}

// Uninstall stops the running service and removes the platform service file.
// Idempotent: missing files are fine.
func Uninstall() error {
	switch runtime.GOOS {
	case "darwin":
		return uninstallMacOS()
	case "linux":
		return uninstallLinux()
	default:
		return fmt.Errorf("unsupported platform: %s", runtime.GOOS)
	}
}

// --- macOS launchd ---

func macOSPlistPath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, "Library", "LaunchAgents", launchdLabel+".plist"), nil
}

func installMacOS(execPath string) (*InstallResult, error) {
	plistPath, err := macOSPlistPath()
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(filepath.Dir(plistPath), 0o755); err != nil {
		return nil, err
	}
	plist := fmt.Sprintf(`<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
    <key>Label</key>
    <string>%s</string>
    <key>ProgramArguments</key>
    <array>
        <string>%s</string>
        <string>agent</string>
        <string>start</string>
    </array>
    <key>RunAtLoad</key>
    <true/>
    <key>KeepAlive</key>
    <true/>
    <key>StandardOutPath</key>
    <string>%s</string>
    <key>StandardErrorPath</key>
    <string>%s</string>
</dict>
</plist>
`,
		launchdLabel,
		execPath,
		filepath.Join(agentLogDir(), stdoutLogName),
		filepath.Join(agentLogDir(), stderrLogName),
	)
	if err := os.WriteFile(plistPath, []byte(plist), 0o644); err != nil {
		return nil, fmt.Errorf("write plist: %w", err)
	}

	// Try to load immediately so the user doesn't have to log out. Errors here
	// are non-fatal — the plist is on disk and will load at next login.
	_ = runQuiet("launchctl", "unload", plistPath) // ignore if not loaded
	loadCmd := fmt.Sprintf("launchctl load %s", plistPath)
	if err := runQuiet("launchctl", "load", plistPath); err != nil {
		return &InstallResult{
			Platform: "darwin",
			UnitPath: plistPath,
			LoadCmd:  loadCmd,
		}, fmt.Errorf("plist written but launchctl load failed: %w (run manually: %s)", err, loadCmd)
	}
	return &InstallResult{
		Platform: "darwin",
		UnitPath: plistPath,
		LoadCmd:  loadCmd,
	}, nil
}

func uninstallMacOS() error {
	plistPath, err := macOSPlistPath()
	if err != nil {
		return err
	}
	// Best-effort unload before deleting.
	_ = runQuiet("launchctl", "unload", plistPath)
	if err := os.Remove(plistPath); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}

// --- Linux systemd --user ---

func linuxUnitPath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".config", "systemd", "user", systemdUnitID+".service"), nil
}

func installLinux(execPath string) (*InstallResult, error) {
	unitPath, err := linuxUnitPath()
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(filepath.Dir(unitPath), 0o755); err != nil {
		return nil, err
	}
	unit := fmt.Sprintf(`[Unit]
Description=Nuzur Local Agent
After=network.target

[Service]
Type=simple
ExecStart=%s agent start
Restart=on-failure
RestartSec=5

[Install]
WantedBy=default.target
`, execPath)
	if err := os.WriteFile(unitPath, []byte(unit), 0o644); err != nil {
		return nil, fmt.Errorf("write unit: %w", err)
	}

	loadCmd := fmt.Sprintf("systemctl --user enable --now %s", systemdUnitID)
	_ = runQuiet("systemctl", "--user", "daemon-reload")
	if err := runQuiet("systemctl", "--user", "enable", "--now", systemdUnitID); err != nil {
		return &InstallResult{
			Platform: "linux",
			UnitPath: unitPath,
			LoadCmd:  loadCmd,
		}, fmt.Errorf("unit written but systemctl failed: %w (run manually: %s)", err, loadCmd)
	}
	return &InstallResult{
		Platform: "linux",
		UnitPath: unitPath,
		LoadCmd:  loadCmd,
	}, nil
}

func uninstallLinux() error {
	unitPath, err := linuxUnitPath()
	if err != nil {
		return err
	}
	_ = runQuiet("systemctl", "--user", "disable", "--now", systemdUnitID)
	if err := os.Remove(unitPath); err != nil && !os.IsNotExist(err) {
		return err
	}
	_ = runQuiet("systemctl", "--user", "daemon-reload")
	return nil
}

// runQuiet runs a shell command and returns any error, suppressing output.
// Used for launchctl / systemctl invocations where we want to know if it
// failed but not flood the user's terminal with normal output.
func runQuiet(name string, args ...string) error {
	cmd := exec.Command(name, args...)
	return cmd.Run()
}
