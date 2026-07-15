// Package deploy implements `nuzur deploy` / `nuzur destroy`: it provisions a
// Linux server, self-hosts the project's database on it (localhost-only), runs
// the generated API, and pairs the box back to nuzur via an outbound local
// agent — so the database is fully managed in nuzur with no inbound DB ports.
//
// The package is transport-agnostic at its core: every provider runs the same
// bootstrap over SSH. A Provisioner supplies only the create-VM / firewall /
// destroy slice; the bring-your-own-server (SSH) provider implements that with
// no provider API and doubles as the universal path for any Linux host.
package deploy

import "context"

// Provider identifies how the target server is obtained/managed.
type Provider string

const (
	// ProviderSSH is bring-your-own-server: the user supplies an existing host.
	ProviderSSH Provider = "ssh"
)

// DBEngine is the database engine. Both MySQL and Postgres are supported as a
// self-hosted local tier (installed + provisioned on the box) and as an external
// (--db-dsn) database the app/agent connect to directly.
type DBEngine string

const (
	DBMySQL    DBEngine = "mysql"
	DBPostgres DBEngine = "postgres"
)

// Target is a resolved server the bootstrap runs against.
type Target struct {
	Host string // IP or hostname
	User string // SSH user (e.g. root)
	Port int    // SSH port (default 22)
	// KeyPath is an optional explicit private key; empty uses the ssh-agent /
	// ~/.ssh/config resolution.
	KeyPath string
}

// Spec is the fully-resolved input to a deployment.
type Spec struct {
	Provider           Provider
	Target             Target
	Identifier         string // app identifier (image/service name, DB name)
	ProjectUUID        string
	ProjectVersionUUID string
	DBEngine           DBEngine
	// ProvisioningToken is minted by the caller (product IssueProvisioningToken)
	// and placed on the box for headless agent pairing.
	ProvisioningToken string
	// SourceDir is the local directory of generated app source (from the
	// go-code-gen extension) that gets copied to the box and built there.
	SourceDir string
}

// Provisioner is the per-provider seam: everything else (the bootstrap) is
// shared. For BYO-SSH these are near-trivial; a cloud adapter implements them
// via the provider's CLI.
type Provisioner interface {
	// Provision returns a reachable Target. BYO-SSH validates and returns the
	// user-supplied host; a cloud provider creates a VM and returns its address.
	Provision(ctx context.Context, spec Spec) (Target, error)
	// ConfigureFirewall opens only the API (443) and SSH (22) ports.
	ConfigureFirewall(ctx context.Context, t Target) error
	// Destroy tears down provider-created infrastructure. BYO-SSH is a no-op
	// (the user owns the box); a cloud provider deletes the VM.
	Destroy(ctx context.Context, t Target) error
}
