package deploy

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// DigitalOcean adapter — shells out to `doctl` (already authenticated by the
// user). Creates a droplet, restricts inbound with a cloud firewall, and deletes
// the droplet on destroy.

const (
	doCLI          = "doctl"
	doDefaultSize  = "s-1vcpu-1gb"
	doDefaultImage = "ubuntu-22-04-x64"
	doSSHReadyWait = 3 * time.Minute
)

type DigitalOceanProvisioner struct{}

func NewDigitalOceanProvisioner() *DigitalOceanProvisioner { return &DigitalOceanProvisioner{} }

func (p *DigitalOceanProvisioner) Provision(ctx context.Context, spec Spec) (Provisioned, error) {
	if err := ensureProviderCLI(ctx, doCLI, []string{"account", "get", "--no-header"},
		"install it from https://docs.digitalocean.com/reference/doctl/ and run `doctl auth init`"); err != nil {
		return Provisioned{}, err
	}
	cfg := spec.ProviderConfig
	if strings.TrimSpace(cfg.Region) == "" {
		return Provisioned{}, fmt.Errorf("--region is required for DigitalOcean (e.g. nyc3, fra1)")
	}
	size := firstNonEmptyStr(cfg.Size, doDefaultSize)
	image := firstNonEmptyStr(cfg.Image, doDefaultImage)

	sshKey, err := p.ensureSSHKey(ctx, spec)
	if err != nil {
		return Provisioned{}, err
	}

	name := "nuzur-" + spec.Identifier
	out, err := runCLI(ctx, doCLI, "compute", "droplet", "create", name,
		"--region", cfg.Region, "--size", size, "--image", image,
		"--ssh-keys", sshKey, "--wait",
		"--format", "ID,PublicIPv4", "--no-header")
	if err != nil {
		return Provisioned{}, fmt.Errorf("creating droplet: %w", err)
	}
	fields := strings.Fields(out)
	if len(fields) < 2 || fields[0] == "" || fields[1] == "" {
		return Provisioned{}, fmt.Errorf("could not parse droplet id/ip from doctl output: %q", out)
	}
	target := Target{Host: fields[1], User: "root", Port: 22, KeyPath: spec.Target.KeyPath}
	if err := sshReady(ctx, target, doSSHReadyWait); err != nil {
		return Provisioned{}, err
	}
	return Provisioned{Target: target, InstanceID: fields[0], Region: cfg.Region}, nil
}

// ensureSSHKey returns a droplet-create --ssh-keys value (a key id or
// fingerprint). It reuses --ssh-key-name if given (resolving a name to its id, or
// passing an id/fingerprint straight through), else uploads the local public key.
func (p *DigitalOceanProvisioner) ensureSSHKey(ctx context.Context, spec Spec) (string, error) {
	name := strings.TrimSpace(spec.ProviderConfig.SSHKeyName)
	if id := p.findSSHKeyID(ctx, name); id != "" {
		return id, nil
	}
	if name != "" {
		// Not found by name — assume the user passed an id or fingerprint.
		return name, nil
	}
	pub, err := resolveSSHPublicKey(spec.Target.KeyPath)
	if err != nil {
		return "", err
	}
	id, err := runCLI(ctx, doCLI, "compute", "ssh-key", "create", doKeyName,
		"--public-key", pub, "--format", "ID", "--no-header")
	if err != nil {
		return "", fmt.Errorf("uploading SSH key to DigitalOcean: %w", err)
	}
	return strings.TrimSpace(id), nil
}

const doKeyName = "nuzur-deploy"

// findSSHKeyID resolves a DO ssh-key name to its numeric id, or "" if not found.
// An empty name looks up the default nuzur-deploy key.
func (p *DigitalOceanProvisioner) findSSHKeyID(ctx context.Context, name string) string {
	want := name
	if want == "" {
		want = doKeyName
	}
	out, err := runCLI(ctx, doCLI, "compute", "ssh-key", "list", "--format", "ID,Name", "--no-header")
	if err != nil {
		return ""
	}
	for _, line := range strings.Split(out, "\n") {
		f := strings.Fields(line)
		if len(f) >= 2 && f[1] == want {
			return f[0]
		}
	}
	return ""
}

func (p *DigitalOceanProvisioner) ConfigureFirewall(ctx context.Context, prov Provisioned, rules []FirewallRule) error {
	inbound := doInboundRules(rules)
	if inbound == "" {
		return nil
	}
	name := "nuzur-fw-" + prov.InstanceID
	_, err := runCLI(ctx, doCLI, "compute", "firewall", "create",
		"--name", name,
		"--inbound-rules", inbound,
		"--outbound-rules", doOutboundAllowAll,
		"--droplet-ids", prov.InstanceID)
	if err != nil && !strings.Contains(err.Error(), "already exists") {
		return fmt.Errorf("creating DigitalOcean firewall: %w", err)
	}
	return nil
}

func (p *DigitalOceanProvisioner) Destroy(ctx context.Context, prov Provisioned) error {
	if strings.TrimSpace(prov.InstanceID) == "" {
		return nil
	}
	if fwID := p.findFirewallID(ctx, "nuzur-fw-"+prov.InstanceID); fwID != "" {
		_, _ = runCLI(ctx, doCLI, "compute", "firewall", "delete", fwID, "--force")
	}
	if _, err := runCLI(ctx, doCLI, "compute", "droplet", "delete", prov.InstanceID, "--force"); err != nil {
		return fmt.Errorf("deleting droplet %s: %w", prov.InstanceID, err)
	}
	return nil
}

func (p *DigitalOceanProvisioner) findFirewallID(ctx context.Context, name string) string {
	out, err := runCLI(ctx, doCLI, "compute", "firewall", "list", "--format", "ID,Name", "--no-header")
	if err != nil {
		return ""
	}
	for _, line := range strings.Split(out, "\n") {
		f := strings.Fields(line)
		if len(f) >= 2 && f[1] == name {
			return f[0]
		}
	}
	return ""
}

const doOutboundAllowAll = "protocol:tcp,ports:all,address:0.0.0.0/0,address:::/0 " +
	"protocol:udp,ports:all,address:0.0.0.0/0,address:::/0 " +
	"protocol:icmp,address:0.0.0.0/0,address:::/0"

// doInboundRules renders FirewallRules as doctl's space-separated inbound-rules
// string (all sources allowed; the on-box ufw is the precise gate).
func doInboundRules(rules []FirewallRule) string {
	var parts []string
	for _, r := range rules {
		if r.Port <= 0 {
			continue
		}
		ports := strconv.Itoa(r.Port)
		if r.PortEnd > 0 {
			ports = fmt.Sprintf("%d-%d", r.Port, r.PortEnd)
		}
		parts = append(parts, fmt.Sprintf("protocol:tcp,ports:%s,address:0.0.0.0/0,address:::/0", ports))
	}
	return strings.Join(parts, " ")
}

// firstNonEmptyStr returns the first non-empty string (a small local helper so
// the deploy package doesn't depend on the app package's firstNonEmpty).
func firstNonEmptyStr(vals ...string) string {
	for _, v := range vals {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}
