package deploy

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// cloud.go holds the pieces every managed-provisioning adapter reuses: running
// the provider CLI, checking it's installed + authed, resolving the SSH public
// key to register, waiting for a fresh VM's SSH to come up, and naming the
// resources we create. Adapters shell out to the provider's own CLI so nuzur
// never handles provider credentials.

// providerNameMaxLen bounds generated resource names. Linode caps labels at 32
// characters — the tightest of the providers — so everything is built to fit
// there and is then valid everywhere else.
const providerNameMaxLen = 32

// providerNameSuffix returns the random tail of a generated resource name. It's a
// package var so tests can pin it.
var providerNameSuffix = func() (string, error) {
	b := make([]byte, 3) // 6 hex chars
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("generating a resource-name suffix: %w", err)
	}
	return hex.EncodeToString(b), nil
}

// providerResourceName builds the name for a resource we create on a provider (a
// VM, an Azure resource group, …): "nuzur-<identifier>-<random>".
//
// The random tail is the point. A bare "nuzur-<identifier>" collides with
// anything the user already named that way — and the failure modes are not
// symmetric:
//
//   - Most providers reject a duplicate name, so the deploy fails confusingly.
//   - AZURE IS WORSE: `az group create` is an idempotent ARM PUT, so a colliding
//     name silently ADOPTS the user's existing resource group — and the matching
//     `nuzur-cli destroy` would then `az group delete` it, taking everything inside
//     with it.
//
// Nothing needs to re-derive this name: it's returned in Provisioned (as the
// InstanceID for GCP/Azure) and persisted on the deployment record, and a managed
// re-deploy always creates a fresh VM anyway.
func providerResourceName(identifier string) (string, error) {
	suffix, err := providerNameSuffix()
	if err != nil {
		return "", err
	}
	base := sanitizeProviderName(identifier)
	if base == "" {
		base = "app"
	}
	// Reserve room for the "nuzur-" prefix and the "-<suffix>" tail.
	if max := providerNameMaxLen - len("nuzur-") - 1 - len(suffix); len(base) > max {
		base = strings.Trim(base[:max], "-")
	}
	return "nuzur-" + base + "-" + suffix, nil
}

// ProviderResourceName mints the provider-side name for a deploy's resources. The
// deploy command calls this before provisioning so the name can be persisted ahead
// of the create call; adapters get it via Spec.ResourceName.
func ProviderResourceName(identifier string) (string, error) {
	return providerResourceName(identifier)
}

// specResourceName returns the name the caller minted for this deploy, falling
// back to minting one. The normal deploy path always mints it up front (so it is
// on disk before the create call); the fallback keeps direct and test callers,
// which have no state to write, working unchanged.
func specResourceName(spec Spec) (string, error) {
	if n := strings.TrimSpace(spec.ResourceName); n != "" {
		return n, nil
	}
	return providerResourceName(spec.Identifier)
}

// reportInstance hands a just-created VM to the caller so it can be persisted
// while the deploy is still running. Adapters must call this as soon as the
// provider acknowledges the instance and BEFORE waiting for SSH — see
// Spec.OnInstanceCreated.
func reportInstance(spec Spec, ref InstanceRef) {
	if spec.OnInstanceCreated != nil {
		spec.OnInstanceCreated(ref)
	}
}

// sanitizeProviderName reduces an identifier to the intersection every provider
// accepts: lowercase alphanumerics and single dashes.
func sanitizeProviderName(id string) string {
	var b strings.Builder
	lastDash := false
	for _, r := range strings.ToLower(strings.TrimSpace(id)) {
		switch {
		case (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9'):
			b.WriteRune(r)
			lastDash = false
		case !lastDash && b.Len() > 0:
			b.WriteRune('-')
			lastDash = true
		}
	}
	return strings.Trim(b.String(), "-")
}

// cliRunner executes a provider CLI and returns trimmed stdout. It is a package
// var so tests can stub provider calls without a real cloud account.
var cliRunner = func(ctx context.Context, name string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		detail := strings.TrimSpace(stderr.String())
		if detail == "" {
			detail = strings.TrimSpace(stdout.String())
		}
		return strings.TrimSpace(stdout.String()), fmt.Errorf("%s %s: %w: %s", name, strings.Join(args, " "), err, detail)
	}
	return strings.TrimSpace(stdout.String()), nil
}

// lookPath is exec.LookPath as a package var so tests can stub CLI presence.
var lookPath = exec.LookPath

// runCLI runs a provider CLI command and returns its trimmed stdout.
func runCLI(ctx context.Context, name string, args ...string) (string, error) {
	return cliRunner(ctx, name, args...)
}

// ensureProviderCLI verifies the provider CLI is installed and (optionally)
// authenticated, returning a friendly, actionable error otherwise. authCheck is
// a cheap "am I logged in" command (e.g. ["account","get"] for doctl).
func ensureProviderCLI(ctx context.Context, bin string, authCheck []string, installHint string) error {
	if _, err := lookPath(bin); err != nil {
		return fmt.Errorf("the %q CLI is required for this provider but wasn't found on PATH — %s", bin, installHint)
	}
	if len(authCheck) > 0 {
		if _, err := runCLI(ctx, bin, authCheck...); err != nil {
			return fmt.Errorf("%q is installed but not authenticated (run its login first): %w", bin, err)
		}
	}
	return nil
}

// resolveSSHPublicKey returns the OpenSSH public key to register with a provider,
// derived from the private key path (its `.pub` sibling) or the user's default
// keys. Used only when the caller didn't reference an already-registered key
// (--ssh-key-name).
func resolveSSHPublicKey(privateKeyPath string) (string, error) {
	var candidates []string
	if strings.TrimSpace(privateKeyPath) != "" {
		candidates = append(candidates, privateKeyPath+".pub")
	} else if home, err := os.UserHomeDir(); err == nil {
		for _, name := range []string{"id_ed25519", "id_rsa"} {
			candidates = append(candidates, filepath.Join(home, ".ssh", name+".pub"))
		}
	}
	for _, c := range candidates {
		if data, err := os.ReadFile(c); err == nil {
			if key := strings.TrimSpace(string(data)); key != "" {
				return key, nil
			}
		}
	}
	return "", fmt.Errorf("couldn't find an SSH public key (looked for %s) — pass --ssh-key <private-key> whose .pub exists, or --ssh-key-name to reference a key already registered with the provider",
		strings.Join(candidates, ", "))
}

// sshDialTimeout / sshReadyPoll pace waitForSSH.
var (
	sshReadyPoll   = 5 * time.Second
	sshDialTimeout = 5 * time.Second
)

// sshReady is the SSH-readiness wait adapters call before returning; a package
// var so tests can stub the network wait.
var sshReady = waitForSSH

// waitForSSH blocks until the target's SSH port accepts a TCP connection or the
// timeout elapses. A fresh cloud VM needs a moment before sshd is up; the deploy
// path's own Ping (which also checks key auth) runs once and doesn't retry, so
// adapters call this before returning.
func waitForSSH(ctx context.Context, t Target, timeout time.Duration) error {
	port := t.Port
	if port == 0 {
		port = 22
	}
	addr := net.JoinHostPort(t.Host, strconv.Itoa(port))
	deadline := time.Now().Add(timeout)
	var lastErr error
	for {
		d := net.Dialer{Timeout: sshDialTimeout}
		conn, err := d.DialContext(ctx, "tcp", addr)
		if err == nil {
			_ = conn.Close()
			return nil
		}
		lastErr = err
		if time.Now().After(deadline) {
			return fmt.Errorf("timed out waiting for SSH on %s after %s: %w", addr, timeout, lastErr)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(sshReadyPoll):
		}
	}
}
