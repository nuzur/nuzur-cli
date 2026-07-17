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

// CopyDir copies localDir's CONTENTS to remotePath on the host, by streaming a
// gzipped tar over ssh.
//
// This used to be `scp -r`, which transfers each file in its own SFTP
// round-trip. A generated app is ~650 small source files, so the copy was
// latency-bound rather than bandwidth-bound — on a transatlantic link it crawled
// at a few KB/s and took many minutes to move ~4MB. One tar stream is a single
// round-trip, and Go source gzips ~5-10x, so the same payload ships in seconds.
//
// `tar -C localDir .` (contents, not the directory itself) matches the old
// scp -r semantics: the caller passes a non-existent remotePath and expects it to
// become a copy of localDir.
func (r *SSHRunner) CopyDir(ctx context.Context, localDir, remotePath string) error {
	quoted := shellSingleQuote(remotePath)
	remote := fmt.Sprintf("mkdir -p %s && tar xzf - -C %s", quoted, quoted)

	sshCmd := exec.CommandContext(ctx, "ssh", r.sshArgs(remote)...)
	tarCmd := exec.CommandContext(ctx, "tar", "czf", "-", "-C", localDir, ".")
	// macOS tar encodes extended attributes as AppleDouble "._*" entries, which
	// GNU tar on the box extracts as REAL files — 741 of them for a generated app,
	// straight into the docker build context. COPYFILE_DISABLE stops bsdtar
	// emitting them; GNU tar ignores the variable, so this is safe everywhere.
	tarCmd.Env = append(os.Environ(), "COPYFILE_DISABLE=1")

	stdin, err := sshCmd.StdinPipe()
	if err != nil {
		return fmt.Errorf("piping tar to ssh: %w", err)
	}
	tarCmd.Stdout = stdin
	tarCmd.Stderr = r.stderr()
	sshCmd.Stdout = r.stderr()
	sshCmd.Stderr = r.stderr()

	if err := sshCmd.Start(); err != nil {
		stdin.Close()
		return fmt.Errorf("starting ssh for the source copy: %w", err)
	}
	// tar writes into ssh's stdin; closing it afterwards is what EOFs the remote
	// tar, so it must happen before we wait on ssh.
	tarErr := tarCmd.Run()
	stdin.Close()
	if waitErr := sshCmd.Wait(); waitErr != nil {
		return fmt.Errorf("copying %s -> %s failed: %w", localDir, remotePath, waitErr)
	}
	if tarErr != nil {
		return fmt.Errorf("archiving %s failed: %w", localDir, tarErr)
	}
	return nil
}

// shellSingleQuote makes s safe as a single-quoted POSIX shell word.
func shellSingleQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
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

// FindInstanceByName finds nothing for BYO-SSH: nuzur created no instance, so
// there is never one of ours to recover.
func (p *SSHProvisioner) FindInstanceByName(ctx context.Context, name, region string) (string, error) {
	return "", nil
}
