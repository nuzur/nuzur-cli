package deploy

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strconv"
	"strings"
)

// RemoteRunner is what the deploy pipeline needs from the box it is deploying
// to: reach it, run things on it, read something back, and ship a directory.
//
// It exists so the pipeline can be exercised without a real host. *SSHRunner is
// the production implementation and the only one in the binary; the interface is
// the seam, not an abstraction over several transports.
type RemoteRunner interface {
	Ping(ctx context.Context) error
	RunCommand(ctx context.Context, command string) error
	// RunScript runs a script on the box. label names WHICH script, and is the
	// noun the failure is reported with — see the ScriptBootstrap/ScriptTeardown
	// constants and RunScript's own doc.
	RunScript(ctx context.Context, label, script string) error
	Capture(ctx context.Context, command string) (string, error)
	CopyDir(ctx context.Context, localDir, remotePath string) error
	// SetSudo runs privileged remote work through `sudo` (see SSHRunner.Sudo).
	// A method rather than a field so the interface can carry it.
	SetSudo(sudo bool)
}

var _ RemoteRunner = (*SSHRunner)(nil)

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
	// os.Stderr. An io.Writer rather than an *os.File so the app layer can point
	// it at the CLI's own stderr sink (outputtools.Stderr), which a test can swap.
	Stderr io.Writer
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

func (r *SSHRunner) stderr() io.Writer {
	if r.Stderr != nil {
		return r.Stderr
	}
	return os.Stderr
}

// SetSudo satisfies RemoteRunner; the field stays exported for direct callers.
func (r *SSHRunner) SetSudo(sudo bool) { r.Sudo = sudo }

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

// The labels RunScript is called with. They name the script in the error the
// caller (and the deployment record's last_error, and the terminal) reads, so a
// teardown failure can never be reported as a bootstrap one.
const (
	ScriptBootstrap = "bootstrap"
	ScriptTeardown  = "teardown"
)

// outputTailBytes is how much of a script's output is kept for the failure
// message. Small on purpose: it is the diagnosis, not the log — the whole stream
// has already been printed live.
const outputTailBytes = 4096

// RunScript pipes a script to `bash -s` (or `sudo bash -s`) on the remote host.
// The script runs with `set -euo pipefail` semantics if it declares them itself.
//
// label names the script ("bootstrap", "teardown"). It is a parameter rather
// than a constant string because ONE runner serves both: the deploy's bootstrap
// and destroy's teardown. A hardcoded noun here meant `destroy` reported a dead
// box as `remote bootstrap script failed`, naming a step that does not exist in
// a destroy.
//
// A failure carries the cause, not just the exit code. ssh writes its own
// diagnosis ("ssh: connect to host H port 22: Operation timed out") and the
// remote script writes its ("E: The package cache file is corrupted") to the
// stream that goes to the terminal — several lines above, and unconnected to
// the error the caller ends up printing. Both are worth more than the
// `exit status 255` they used to be replaced by, so the tail of that stream
// rides along in the error itself.
func (r *SSHRunner) RunScript(ctx context.Context, label, script string) error {
	shell := "bash -s"
	if r.Sudo {
		// Requires passwordless sudo so no prompt consumes the piped script.
		shell = "sudo bash -s"
	}
	cmd := exec.CommandContext(ctx, "ssh", r.sshArgs(shell)...)
	cmd.Stdin = strings.NewReader(script)
	tail := &tailBuffer{max: outputTailBytes}
	// ONE writer for both streams, and the SAME io.Writer value in both fields.
	// os/exec compares them: equal means the child gets one fd and the parent
	// runs one copying goroutine, which is what makes the merge the user sees on
	// the terminal. Two different writers over the same sink would be two
	// goroutines writing it concurrently — and if that sink is a bytes.Buffer,
	// io.Copy takes its ReadFrom path, which re-slices the buffer and silently
	// eats what the other goroutine appended.
	//
	// The tail is a copy of that merged stream, so nothing the user saw is lost
	// and the diagnosis quoted below is literally the last thing they read.
	merged := io.MultiWriter(r.stderr(), tail)
	cmd.Stdout = merged
	cmd.Stderr = merged
	if err := cmd.Run(); err != nil {
		if cause := lastLines(tail.String(), 2); cause != "" {
			return fmt.Errorf("remote %s script failed: %w: %s", scriptLabel(label), err, cause)
		}
		return fmt.Errorf("remote %s script failed: %w", scriptLabel(label), err)
	}
	return nil
}

// scriptLabel keeps an unlabeled call readable rather than emitting "remote
// script failed" with a double space.
func scriptLabel(label string) string {
	label = strings.TrimSpace(label)
	if label == "" {
		return "remote"
	}
	return label
}

// tailBuffer keeps only the LAST max bytes written to it. A bootstrap streams
// an entire docker build through stderr, so an unbounded buffer would hold
// megabytes to quote three lines of it.
type tailBuffer struct {
	max int
	buf []byte
}

func (t *tailBuffer) Write(p []byte) (int, error) {
	n := len(p)
	if len(p) > t.max {
		p = p[len(p)-t.max:]
	}
	t.buf = append(t.buf, p...)
	if len(t.buf) > t.max {
		t.buf = t.buf[len(t.buf)-t.max:]
	}
	return n, nil
}

func (t *tailBuffer) String() string { return string(t.buf) }

// lastLines returns the last n non-blank lines of s, joined with "; ", for use
// inside a one-line error. A truncated leading line (the tail buffer cuts
// mid-line) is dropped by taking whole lines from the end only.
func lastLines(s string, n int) string {
	lines := strings.Split(s, "\n")
	kept := []string{}
	for i := len(lines) - 1; i >= 0 && len(kept) < n; i-- {
		line := strings.TrimSpace(lines[i])
		if line == "" {
			continue
		}
		kept = append([]string{line}, kept...)
	}
	joined := strings.Join(kept, "; ")
	// A single pathological line (a progress bar, a base64 blob) must not turn
	// the error into a paragraph.
	if len(joined) > 400 {
		joined = joined[:400] + "…"
	}
	return joined
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
