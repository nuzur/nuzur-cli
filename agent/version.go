package agent

import (
	"strings"

	"golang.org/x/mod/semver"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// isFailedPreconditionStream reports whether an error returned from a gRPC
// stream operation carries the FailedPrecondition status. Used to detect
// the server-side version rejection so the daemon can exit non-retryable
// instead of bouncing against the same wall every backoff cycle.
func isFailedPreconditionStream(err error) bool {
	if err == nil {
		return false
	}
	if s, ok := status.FromError(err); ok {
		return s.Code() == codes.FailedPrecondition
	}
	// status.FromError unwraps gRPC errors directly but not wrapped ones.
	// Fall back to a string-contains as a last resort so future error
	// wrapping doesn't silently break the check.
	return strings.Contains(err.Error(), codes.FailedPrecondition.String())
}

// cliVersionAtLeast mirrors the server-side comparison (see
// connection-manager/server/local_agent_version.go). Kept here to avoid
// pulling in the cm server package as a dependency from the CLI.
//
// Sentinel "0.0.0" / "" min means the server isn't enforcing — we always
// pass. Unparseable inputs FAIL CLOSED so a malformed Welcome doesn't
// silently let an outdated agent talk to a newer server.
func cliVersionAtLeast(cliVersion, minVersion string) bool {
	if minVersion == "" || minVersion == "0.0.0" || minVersion == "v0.0.0" {
		return true
	}
	if cliVersion == "" {
		return false
	}
	v := normalizeSemver(cliVersion)
	m := normalizeSemver(minVersion)
	if !semver.IsValid(v) || !semver.IsValid(m) {
		return false
	}
	return semver.Compare(v, m) >= 0
}

func normalizeSemver(s string) string {
	if strings.HasPrefix(s, "v") {
		return s
	}
	return "v" + s
}
