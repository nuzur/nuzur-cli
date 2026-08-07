package deploy

import (
	"bytes"
	"context"
	"fmt"
	"os/exec"
	"strings"
)

// LocalCommand runs a command on the machine running the CLI and returns its
// trimmed stdout.
//
// Distinct from cliRunner, which the cloud adapters use, for one reason: dir.
// git and gh answer differently depending on which repository they are standing
// in, and the k8s provider runs them inside the generated workspace — which may
// itself be a repo, or a subdirectory of one.
//
// A package var so tests can stub git/gh without a repository or a network. It
// pairs with LookLocal below; both are replaced together.
var LocalCommand = func(ctx context.Context, dir, name string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Dir = dir
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		detail := strings.TrimSpace(stderr.String())
		if detail == "" {
			detail = strings.TrimSpace(stdout.String())
		}
		return strings.TrimSpace(stdout.String()),
			fmt.Errorf("%s %s: %w: %s", name, strings.Join(args, " "), err, detail)
	}
	return strings.TrimSpace(stdout.String()), nil
}

// LookLocal reports whether a local binary is on PATH. A package var for the
// same reason as LocalCommand.
var LookLocal = func(name string) error {
	_, err := exec.LookPath(name)
	return err
}

// RequireLocalTool returns an actionable error when a tool the k8s flow needs is
// missing, naming what it is needed for rather than just that it is absent.
func RequireLocalTool(name, neededFor, installHint string) error {
	if err := LookLocal(name); err != nil {
		return fmt.Errorf("%q is required to %s but was not found on PATH — %s", name, neededFor, installHint)
	}
	return nil
}
