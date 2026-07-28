package app

import (
	"fmt"
	"os"
	"runtime"
	"strings"
)

// provisioningTokenPrefix mirrors the server's prefix for provisioning tokens
// (nuzur-go product/server/provisioning_token.go). Agent tokens use nzlat_.
const provisioningTokenPrefix = "nzpt_"
const localAgentTokenPrefix = "nzlat_"

// detectHeadless reports whether this machine probably cannot open a browser,
// so pairing should ask for a pasted token instead of starting a login flow
// that would either fail outright or hang waiting on a callback nobody can
// reach.
func detectHeadless() bool {
	return shouldUseHeadlessPairing(runtime.GOOS, os.Getenv)
}

// shouldUseHeadlessPairing is the pure core of detectHeadless.
//
// An SSH session means the browser would open on the wrong machine — the
// server's, not the one in front of the user — so it counts as headless on
// every OS. On Linux, no display server is the other giveaway.
func shouldUseHeadlessPairing(goos string, getenv func(string) string) bool {
	if getenv("SSH_CONNECTION") != "" || getenv("SSH_TTY") != "" {
		return true
	}
	if goos == "linux" {
		return getenv("DISPLAY") == "" && getenv("WAYLAND_DISPLAY") == ""
	}
	return false
}

// validateProvisioningToken checks a pasted token looks like a provisioning
// token before spending a round-trip on it.
func validateProvisioningToken(s string) error {
	s = strings.TrimSpace(s)
	switch {
	case s == "":
		return fmt.Errorf("paste the token from the pairing page")
	case strings.HasPrefix(s, localAgentTokenPrefix):
		// Both live in the same neighbourhood of the docs; saying which one
		// this is saves the user a confusing "invalid token" round-trip.
		return fmt.Errorf("that looks like an agent token (%s…), not a pairing token — copy the one shown on the pairing page, which starts with %s",
			localAgentTokenPrefix, provisioningTokenPrefix)
	case !strings.HasPrefix(s, provisioningTokenPrefix):
		return fmt.Errorf("a pairing token starts with %s", provisioningTokenPrefix)
	case len(s) < len(provisioningTokenPrefix)+16:
		return fmt.Errorf("that token looks truncated — copy the whole value")
	}
	return nil
}
