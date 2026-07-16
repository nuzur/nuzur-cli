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
	// It doubles as the universal fallback for any Linux box.
	ProviderSSH Provider = "ssh"
	// Managed providers create the VM for the user by shelling out to the
	// provider's own (already-authenticated) CLI.
	ProviderDigitalOcean Provider = "digitalocean"
	ProviderHetzner      Provider = "hetzner"
	ProviderAWS          Provider = "aws"
	ProviderGCP          Provider = "gcp"
	ProviderAzure        Provider = "azure"
	ProviderVultr        Provider = "vultr"
	ProviderLinode       Provider = "linode"
	ProviderScaleway     Provider = "scaleway"
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

// ProviderConfig holds the managed-provisioning inputs for a cloud provider.
// Ignored by the BYO-SSH provider. Provider auth is deliberately NOT here — the
// adapters shell out to the user's already-authenticated provider CLI, so nuzur
// never handles provider tokens.
type ProviderConfig struct {
	Region     string // provider region/location (e.g. "nyc3", "nbg1")
	Size       string // instance size/type (provider-specific; empty → adapter default)
	Image      string // OS image (empty → adapter default, an Ubuntu LTS)
	SSHKeyName string // name/id of an SSH key already registered with the provider;
	// empty → the adapter uploads the public half of Target.KeyPath (or the default key).
}

// Spec is the fully-resolved input to a deployment.
type Spec struct {
	Provider           Provider
	Target             Target
	ProviderConfig     ProviderConfig // managed-provisioning inputs (cloud providers only)
	Identifier         string         // app identifier (image/service name, DB name)
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

// Provisioned is the result of Provision: a reachable Target plus the identifiers
// a cloud teardown needs. For BYO-SSH, InstanceID/Region are empty (nothing to
// delete). These are persisted on the Deployment record so `destroy` can find and
// delete the VM later.
type Provisioned struct {
	Target     Target
	InstanceID string // provider VM/instance id
	Region     string // provider region the VM lives in
}

// FirewallRule is one inbound-TCP allowance for a cloud provider firewall. A
// single port sets Port (PortEnd == 0); a contiguous range sets both. These
// mirror the box's own ufw rules (SSH + the Caddy front doors) as defense in
// depth — the on-box ufw remains the authoritative gate.
type FirewallRule struct {
	Port    int
	PortEnd int // inclusive range end; 0 → single port
}

// Provisioner is the per-provider seam: everything else (the bootstrap) is
// shared. For BYO-SSH these are near-trivial; a cloud adapter implements them by
// shelling out to the provider's CLI.
type Provisioner interface {
	// Provision returns a reachable, SSH-ready Target. BYO-SSH validates and
	// returns the user-supplied host; a cloud provider creates a VM, waits for
	// SSH, and returns its address + instance id.
	Provision(ctx context.Context, spec Spec) (Provisioned, error)
	// ConfigureFirewall restricts inbound to the given TCP rules (SSH + the Caddy
	// front doors). BYO-SSH is a no-op (the box's ufw does it); cloud adapters
	// create a provider security group/firewall for the instance.
	ConfigureFirewall(ctx context.Context, p Provisioned, rules []FirewallRule) error
	// Destroy tears down provider-created infrastructure. BYO-SSH is a no-op
	// (the user owns the box); a cloud provider deletes the VM by instance id.
	Destroy(ctx context.Context, p Provisioned) error
}
