package agent

import (
	"context"
	"os/exec"
	"strconv"
	"strings"
	"time"

	"google.golang.org/protobuf/types/known/timestamppb"

	pb "github.com/nuzur/nuzur-cli/protodeps/gen"
)

// deploymentProbeTimeout bounds each individual on-box probe (systemctl / docker
// inspect / DB ping). Kept short: the whole point is a cheap, fast health check.
const deploymentProbeTimeout = 5 * time.Second

// handleCollectDeploymentMetrics answers a pull-based health probe for one
// deployment. Everything here is a cheap read — one `systemctl is-active`, one
// `docker inspect`, one `SELECT 1` (PingContext) — and nothing is stored. The
// response is always a DeploymentMetricsProbeResponse (soft failures land in
// probe_error / false flags); QueryError is reserved for nothing here since an
// unreachable app or DB is a normal reportable state, not an RPC error.
func handleCollectDeploymentMetrics(ctx context.Context, stream pb.NuzurConnectionManager_LocalAgentChannelClient, pool *dbPool, req *pb.DeploymentMetricsProbeRequest) {
	m := &pb.DeploymentMetrics{CollectedAt: timestamppb.Now()}
	var probeErrs []string

	identifier := req.GetIdentifier()

	// App-surface probes only make sense for a full_app deployment (a db_only
	// box has no {identifier}-api service or container).
	if req.GetFullApp() && identifier != "" {
		m.AppServiceActive = systemctlIsActive(ctx, identifier+"-api.service")
		status, restarts, err := dockerInspectStatus(ctx, identifier+"-api")
		if err != nil {
			probeErrs = append(probeErrs, "container: "+err.Error())
		} else {
			m.ContainerStatus = status
			m.RestartCount = restarts
		}
	}

	// DB reachability over the deployment's registered connection. For an
	// external (remote) DB this connects outbound; unreachable is a normal
	// state reported as db_reachable=false, not a probe error.
	if connUUID := req.GetConnectionUuid(); connUUID != "" {
		reachable, err := pingConnection(ctx, pool, connUUID)
		if err != nil {
			probeErrs = append(probeErrs, "db: "+err.Error())
		}
		m.DbReachable = reachable
	}

	if len(probeErrs) > 0 {
		m.ProbeError = strings.Join(probeErrs, "; ")
	}

	_ = stream.Send(&pb.LocalAgentToServer{Message: &pb.LocalAgentToServer_DeploymentMetricsProbeResponse{
		DeploymentMetricsProbeResponse: &pb.DeploymentMetricsProbeResponse{
			RequestId: req.GetRequestId(),
			Metrics:   m,
		},
	}})
}

// systemctlIsActive reports whether a systemd unit is active. `systemctl
// is-active` exits non-zero for inactive units but still prints the state, so
// we ignore the exit code and match on the printed word.
func systemctlIsActive(ctx context.Context, unit string) bool {
	cctx, cancel := context.WithTimeout(ctx, deploymentProbeTimeout)
	defer cancel()
	out, _ := exec.CommandContext(cctx, "systemctl", "is-active", unit).Output()
	return strings.TrimSpace(string(out)) == "active"
}

// dockerInspectStatus returns a container's State.Status ("running", "exited",
// …) and RestartCount in one cheap call. A missing container surfaces as an
// error (the caller records it in probe_error).
func dockerInspectStatus(ctx context.Context, container string) (string, int32, error) {
	cctx, cancel := context.WithTimeout(ctx, deploymentProbeTimeout)
	defer cancel()
	out, err := exec.CommandContext(cctx, "docker", "inspect", "-f", "{{.State.Status}} {{.RestartCount}}", container).Output()
	if err != nil {
		return "", 0, err
	}
	fields := strings.Fields(strings.TrimSpace(string(out)))
	if len(fields) == 0 {
		return "", 0, nil
	}
	status := fields[0]
	var restarts int32
	if len(fields) > 1 {
		if n, convErr := strconv.Atoi(fields[1]); convErr == nil {
			restarts = int32(n)
		}
	}
	return status, restarts, nil
}

// pingConnection opens (or reuses) the pooled handle for connUUID and pings it.
// Returns (false, err) only when the connection can't be resolved/opened; an
// unreachable-but-registered DB returns (false, nil) — a normal reported state.
func pingConnection(ctx context.Context, pool *dbPool, connUUID string) (bool, error) {
	db, err := pool.Get(connUUID)
	if err != nil {
		return false, err
	}
	cctx, cancel := context.WithTimeout(ctx, deploymentProbeTimeout)
	defer cancel()
	if err := db.PingContext(cctx); err != nil {
		return false, nil
	}
	return true, nil
}
