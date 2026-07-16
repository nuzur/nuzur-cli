package app

import (
	"context"
	"net/url"
	"strconv"
	"strings"

	"github.com/nuzur/nuzur-cli/constants"
	"github.com/nuzur/nuzur-cli/deploy"
	nemgen "github.com/nuzur/nem/idl/gen"
	"github.com/nuzur/nuzur-cli/productclient"
	pb "github.com/nuzur/nuzur-cli/protodeps/gen"
)

// deploymentReportInput carries the resolved facts about one deploy needed to
// build the cloud-side deployment record. Everything here is already computed in
// runDeploy; this just gathers it so reportDeployment can stay declarative.
type deploymentReportInput struct {
	Runner         *deploy.SSHRunner // for the on-box ports read-back (full-app only)
	Provider       deploy.Provider   // where it's deployed: ssh (BYO) or a managed provider
	Identifier     string
	ProjectUUID    string
	ProjectVersion string
	LocalAgentUUID string
	ConnUUID       string
	Host           string
	DBEngine       deploy.DBEngine
	ExternalDB     bool
	DBOnly         bool
	Domain         string
	PublicURL      string
	DataManagerURL string
	UseHTTPS       bool
	ExtDBPort      string // external DB port (from --db-dsn); empty for self-hosted
	RESTEnabled    bool
	GRPCEnabled    bool
	JWTAuth        bool
	AuthConfig     string // resolved go-code-gen `auth` config value (for keycloak)

	// provider embed (managed-provider provisioning spec)
	Region     string
	Size       string
	Image      string
	SSHKeyName string
	// server embed
	SSHUser string
	SSHPort int
	// database embed
	DBSchema string
	// codegen embed
	Custom    bool
	SourceDir string
	// revision: the uniquely-tagged image built for this deploy
	ImageName string

	// Two-phase progress reporting. The deploy reports IN_PROGRESS as soon as the
	// box exists, then finalizes the SAME revision (RevisionUUID) once the ports,
	// URLs and agent are known — so a half-finished or failed deploy is visible in
	// nuzur instead of silent.
	RevisionUUID  string // empty → insert a new revision; set → update that one
	Status        nemgen.DeploymentRevisionStatus
	StatusMessage string
}

// maxStatusMessage matches the schema's status_message varchar(512).
const maxStatusMessage = 512

func truncateStatusMessage(msg string) string {
	if len(msg) <= maxStatusMessage {
		return msg
	}
	return msg[:maxStatusMessage-1] + "…"
}

// updateDeployRevision moves an in-flight revision along: a status_message per
// phase, or a terminal status. Best-effort by design — progress reporting must
// never fail an otherwise-good deploy, and the local record is authoritative for
// teardown regardless.
func (i *Implementation) updateDeployRevision(ctx context.Context, revisionUUID string, st nemgen.DeploymentRevisionStatus, msg string) {
	if revisionUUID == "" {
		return
	}
	authCtx, err := productclient.ClientContext()
	if err != nil {
		return
	}
	_, _ = i.productClient.ProductClient.UpdateDeploymentRevisionStatus(authCtx, &pb.UpdateDeploymentRevisionStatusRequest{
		RevisionUuid:  revisionUUID,
		Status:        st,
		StatusMessage: truncateStatusMessage(msg),
	})
	_ = ctx
}

// reportDeployment records the deployment in nuzur (UpsertDeployment) so it shows
// up in the web with its config + can be health-checked. Best-effort: the caller
// treats a failure as a warning, since the local deployment JSON remains the
// authoritative source for `destroy`.
//
// ctx is the deploy's long-lived context, used only for the on-box ports
// read-back. The UpsertDeployment RPC gets its OWN fresh short-lived auth
// context (a deploy runs for minutes through the docker build, so any context
// minted at the start of the deploy would already be past its 10s deadline).
// reportDeployment records the deploy in nuzur and returns the revision's uuid,
// so an in-progress report can be finalized (or failed) later via the same
// revision. See deploymentReportInput's two-phase notes.
func (i *Implementation) reportDeployment(ctx context.Context, in deploymentReportInput) (string, error) {
	// Core identity: the stable (user, host, identifier) row. user/status/uuid/
	// audit/active_revision are server-owned.
	d := &nemgen.Deployment{
		ProjectUuid: in.ProjectUUID,
		Identifier:  in.Identifier,
		Host:        in.Host,
	}

	// Revision: the full config that shipped in this deploy, grouped into the four
	// embeds. project_version/cli_version/image live here (they change per deploy);
	// status/deployed_at/audit are server-owned.
	rev := &nemgen.DeploymentRevision{
		ProjectVersionUuid: in.ProjectVersion,
		CliVersion:         constants.CLI_VERSION,
		Provider: &nemgen.DeploymentProvider{
			Name:       deploymentProvider(in.Provider),
			Region:     in.Region,
			Size:       in.Size,
			Image:      in.Image,
			SshKeyName: in.SSHKeyName,
		},
		Server: &nemgen.DeploymentServer{
			SshUser:        in.SSHUser,
			SshPort:        int64(in.SSHPort),
			PublicPort:     publicPortFromURL(in.PublicURL, in.UseHTTPS),
			Domain:         in.Domain,
			PublicUrl:      in.PublicURL,
			DataManagerUrl: in.DataManagerURL,
			LocalAgentUuid: in.LocalAgentUUID,
		},
		Database: &nemgen.DeploymentDatabase{
			Engine:         agentConnDbType(in.DBEngine),
			Location:       deploymentDBLocation(in.ExternalDB),
			Port:           dbPortForRecord(in.ExternalDB, in.ExtDBPort, in.DBEngine),
			Schema:         in.DBSchema,
			ConnectionUuid: in.ConnUUID,
		},
		Codegen: &nemgen.DeploymentCodegen{
			Mode:        deploymentMode(in.DBOnly),
			RestEnabled: in.RESTEnabled && !in.DBOnly,
			GrpcEnabled: in.GRPCEnabled && !in.DBOnly,
			AuthType:    deploymentAuthType(in.JWTAuth, in.AuthConfig),
			Custom:      in.Custom,
			SourceDir:   in.SourceDir,
		},
	}
	if !in.DBOnly {
		rev.Server.ContainerName = in.Identifier + "-api"
		// image_name is recorded only once the image has actually been built (i.e.
		// the finalizing report). An in-progress or failed revision must not claim
		// an image that never existed — the tag is minted before the build, so
		// reporting it early would put a phantom artifact in the history.
		if in.ImageName != "" {
			rev.ImageName = in.ImageName
			rev.Server.ImageName = in.ImageName
		}
		httpP, grpcP, dbP := readBackPorts(ctx, in.Runner, in.Identifier)
		rev.Server.HttpPort = httpP
		rev.Server.GrpcPort = grpcP
		if dbP > 0 {
			rev.Database.Port = dbP
		}
	}

	rev.Status = in.Status
	rev.StatusMessage = truncateStatusMessage(in.StatusMessage)

	authCtx, err := productclient.ClientContext()
	if err != nil {
		return "", err
	}
	res, err := i.productClient.ProductClient.UpsertDeployment(authCtx, &pb.UpsertDeploymentRequest{
		Deployment:   d,
		Revision:     rev,
		RevisionUuid: in.RevisionUUID,
	})
	if err != nil {
		return "", err
	}
	return res.GetActiveRevision().GetUuid(), nil
}

// readBackPorts reads the box-allocated ports the bootstrap recorded at
// /etc/nuzur/{identifier}/ports (http/grpc/db, one KEY=VAL per line). Missing
// file or fields yield zeros — the caller falls back to client-side values.
func readBackPorts(ctx context.Context, runner *deploy.SSHRunner, identifier string) (httpP, grpcP, dbP int64) {
	if runner == nil {
		return 0, 0, 0
	}
	raw, err := runner.Capture(ctx, "cat /etc/nuzur/"+identifier+"/ports 2>/dev/null")
	if err != nil {
		return 0, 0, 0
	}
	for _, line := range strings.Split(raw, "\n") {
		kv := strings.SplitN(strings.TrimSpace(line), "=", 2)
		if len(kv) != 2 {
			continue
		}
		n, _ := strconv.Atoi(strings.TrimSpace(kv[1]))
		switch strings.TrimSpace(kv[0]) {
		case "http":
			httpP = int64(n)
		case "grpc":
			grpcP = int64(n)
		case "db":
			dbP = int64(n)
		}
	}
	return httpP, grpcP, dbP
}

// publicPortFromURL extracts the front-door port from the resolved public URL,
// defaulting to 443 (https) / 80 (http) when the URL carries no explicit port
// (e.g. a --domain deploy).
func publicPortFromURL(publicURL string, useHTTPS bool) int64 {
	if publicURL == "" {
		return 0
	}
	if u, err := url.Parse(publicURL); err == nil {
		if p := u.Port(); p != "" {
			if n, err := strconv.Atoi(p); err == nil {
				return int64(n)
			}
		}
	}
	if useHTTPS {
		return 443
	}
	return 80
}

// dbPortForRecord returns the DB port: the parsed --db-dsn port for an external
// DB, else the self-hosted default for the engine (5432 postgres / 3306 mysql).
func dbPortForRecord(externalDB bool, extPort string, engine deploy.DBEngine) int64 {
	if externalDB {
		if n, err := strconv.Atoi(strings.TrimSpace(extPort)); err == nil {
			return int64(n)
		}
		return 0
	}
	if engine == deploy.DBPostgres {
		return 5432
	}
	return 3306
}

// deploymentProvider records where the deployment runs, defaulting to ssh
// (bring-your-own-server) for older callers / an unset provider.
func deploymentProvider(p deploy.Provider) string {
	if strings.TrimSpace(string(p)) == "" {
		return string(deploy.ProviderSSH)
	}
	return string(p)
}

func deploymentDBLocation(externalDB bool) nemgen.DeploymentDbLocation {
	if externalDB {
		return nemgen.DeploymentDbLocation_DEPLOYMENT_DB_LOCATION_EXTERNAL
	}
	return nemgen.DeploymentDbLocation_DEPLOYMENT_DB_LOCATION_SELF_HOSTED
}

func deploymentMode(dbOnly bool) nemgen.DeploymentMode {
	if dbOnly {
		return nemgen.DeploymentMode_DEPLOYMENT_MODE_DB_ONLY
	}
	return nemgen.DeploymentMode_DEPLOYMENT_MODE_FULL_APP
}

// deploymentAuthType maps the resolved auth config to the record enum. The
// go-code-gen `auth` value is authoritative when present; otherwise a JWT auth
// server detected in the generated base.yaml implies jwt, else none.
func deploymentAuthType(jwtAuth bool, authConfig string) nemgen.DeploymentAuthType {
	switch strings.ToLower(strings.TrimSpace(authConfig)) {
	case "keycloak":
		return nemgen.DeploymentAuthType_DEPLOYMENT_AUTH_TYPE_KEYCLOAK
	case "jwt":
		return nemgen.DeploymentAuthType_DEPLOYMENT_AUTH_TYPE_JWT
	}
	if jwtAuth {
		return nemgen.DeploymentAuthType_DEPLOYMENT_AUTH_TYPE_JWT
	}
	return nemgen.DeploymentAuthType_DEPLOYMENT_AUTH_TYPE_NONE
}
