package deploy

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
)

// SSHRunner executes commands and copies files to a Target by shelling out to
// the system `ssh` / `scp`. This reuses the user's ssh-agent and ~/.ssh/config
// (matching the "shell out" approach) and avoids in-process key handling.
type SSHRunner struct {
	Target Target
	// Sudo runs the bootstrap script through `sudo` (for non-root SSH users on
	// hosts with passwordless sudo). The DB copy still lands in a user-writable
	// path, so only the privileged bootstrap needs it.
	Sudo bool
	// Stderr, when set, receives live command stderr (progress). Defaults to
	// os.Stderr.
	Stderr *os.File
}

func NewSSHRunner(t Target) *SSHRunner {
	if t.Port == 0 {
		t.Port = 22
	}
	if t.User == "" {
		t.User = "root"
	}
	return &SSHRunner{Target: t}
}

func (r *SSHRunner) userHost() string {
	return fmt.Sprintf("%s@%s", r.Target.User, r.Target.Host)
}

// commonOpts are shared ssh/scp options: fail instead of prompting for a
// password (key auth only), and auto-accept a new host key on first connect.
func (r *SSHRunner) commonOpts() []string {
	return []string{
		"-o", "BatchMode=yes",
		"-o", "StrictHostKeyChecking=accept-new",
		"-o", "ConnectTimeout=15",
	}
}

func (r *SSHRunner) sshArgs(extra ...string) []string {
	args := []string{"-p", strconv.Itoa(r.Target.Port)}
	if r.Target.KeyPath != "" {
		args = append(args, "-i", r.Target.KeyPath)
	}
	args = append(args, r.commonOpts()...)
	args = append(args, r.userHost())
	return append(args, extra...)
}

func (r *SSHRunner) stderr() *os.File {
	if r.Stderr != nil {
		return r.Stderr
	}
	return os.Stderr
}

// Ping verifies the host is reachable and key auth works.
func (r *SSHRunner) Ping(ctx context.Context) error {
	if err := r.RunCommand(ctx, "true"); err != nil {
		return fmt.Errorf("ssh preflight to %s failed (check host, user, and key): %w", r.userHost(), err)
	}
	return nil
}

// RunCommand runs a single remote command, streaming its output to stderr.
func (r *SSHRunner) RunCommand(ctx context.Context, command string) error {
	cmd := exec.CommandContext(ctx, "ssh", r.sshArgs(command)...)
	cmd.Stdout = r.stderr()
	cmd.Stderr = r.stderr()
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("remote command failed: %w", err)
	}
	return nil
}

// RunScript pipes a script to `bash -s` (or `sudo bash -s`) on the remote host.
// The script runs with `set -euo pipefail` semantics if it declares them itself.
func (r *SSHRunner) RunScript(ctx context.Context, script string) error {
	shell := "bash -s"
	if r.Sudo {
		// Requires passwordless sudo so no prompt consumes the piped script.
		shell = "sudo bash -s"
	}
	cmd := exec.CommandContext(ctx, "ssh", r.sshArgs(shell)...)
	cmd.Stdin = strings.NewReader(script)
	cmd.Stdout = r.stderr()
	cmd.Stderr = r.stderr()
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("remote bootstrap script failed: %w", err)
	}
	return nil
}

// Capture runs a remote command and returns its stdout (trimmed). Used for
// health checks and status probes.
func (r *SSHRunner) Capture(ctx context.Context, command string) (string, error) {
	cmd := exec.CommandContext(ctx, "ssh", r.sshArgs(command)...)
	var out bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = r.stderr()
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("remote command failed: %w", err)
	}
	return strings.TrimSpace(out.String()), nil
}

// CopyDir copies a local directory to remotePath on the host (recursive scp).
func (r *SSHRunner) CopyDir(ctx context.Context, localDir, remotePath string) error {
	args := []string{"-r", "-P", strconv.Itoa(r.Target.Port)}
	if r.Target.KeyPath != "" {
		args = append(args, "-i", r.Target.KeyPath)
	}
	args = append(args, r.commonOpts()...)
	args = append(args, localDir, fmt.Sprintf("%s:%s", r.userHost(), remotePath))

	cmd := exec.CommandContext(ctx, "scp", args...)
	cmd.Stdout = r.stderr()
	cmd.Stderr = r.stderr()
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("scp %s -> %s failed: %w", localDir, remotePath, err)
	}
	return nil
}

// ─────────────────────────────────────────────
// BYO-SSH provisioner
// ─────────────────────────────────────────────

// SSHProvisioner implements Provisioner for a user-supplied host. It performs no
// provider-API work: the box already exists, the firewall is configured on the
// box by the bootstrap (ufw), and teardown of the box itself is the user's.
type SSHProvisioner struct{}

func NewSSHProvisioner() *SSHProvisioner { return &SSHProvisioner{} }

func (p *SSHProvisioner) Provision(ctx context.Context, spec Spec) (Provisioned, error) {
	t := spec.Target
	if strings.TrimSpace(t.Host) == "" {
		return Provisioned{}, fmt.Errorf("--host is required for the ssh provider")
	}
	if t.User == "" {
		t.User = "root"
	}
	if t.Port == 0 {
		t.Port = 22
	}
	return Provisioned{Target: t}, nil
}

// ConfigureFirewall is a no-op for BYO-SSH: the firewall (ufw, 443+22 only) is
// applied on the box as part of the bootstrap. Cloud adapters use this for
// provider-level security groups.
func (p *SSHProvisioner) ConfigureFirewall(ctx context.Context, prov Provisioned, rules []FirewallRule) error {
	return nil
}

// Destroy is a no-op for BYO-SSH: the user owns the box. Agent revocation and
// local-state cleanup are handled by the destroy command, not the provisioner.
func (p *SSHProvisioner) Destroy(ctx context.Context, prov Provisioned) error { return nil }
