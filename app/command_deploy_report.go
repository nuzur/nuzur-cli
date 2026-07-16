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
func (i *Implementation) reportDeployment(ctx context.Context, in deploymentReportInput) error {
	d := &nemgen.Deployment{
		ProjectUuid:        in.ProjectUUID,
		ProjectVersionUuid: in.ProjectVersion,
		LocalAgentUuid:     in.LocalAgentUUID,
		ConnectionUuid:     in.ConnUUID,
		Identifier:         in.Identifier,
		Host:               in.Host,
		Provider:           string(deploy.ProviderSSH),
		DbEngine:           agentConnDbType(in.DBEngine),
		DbLocation:         deploymentDBLocation(in.ExternalDB),
		Mode:               deploymentMode(in.DBOnly),
		RestEnabled:        in.RESTEnabled && !in.DBOnly,
		GrpcEnabled:        in.GRPCEnabled && !in.DBOnly,
		AuthType:           deploymentAuthType(in.JWTAuth, in.AuthConfig),
		Domain:             in.Domain,
		PublicUrl:          in.PublicURL,
		DataManagerUrl:     in.DataManagerURL,
		PublicPort:         publicPortFromURL(in.PublicURL, in.UseHTTPS),
		DbPort:             dbPortForRecord(in.ExternalDB, in.ExtDBPort, in.DBEngine),
		CliVersion:         constants.CLI_VERSION,
	}
	if !in.DBOnly {
		d.ContainerName = in.Identifier + "-api"
		d.ImageName = "nuzur/" + in.Identifier + ":latest"
		httpP, grpcP, dbP := readBackPorts(ctx, in.Runner, in.Identifier)
		d.HttpPort = httpP
		d.GrpcPort = grpcP
		if dbP > 0 {
			d.DbPort = dbP
		}
	}

	authCtx, err := productclient.ClientContext()
	if err != nil {
		return err
	}
	_, err = i.productClient.ProductClient.UpsertDeployment(authCtx, &pb.UpsertDeploymentRequest{Deployment: d})
	return err
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
