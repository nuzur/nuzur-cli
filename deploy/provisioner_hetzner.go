package deploy

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// Hetzner Cloud adapter — shells out to `hcloud` (already authenticated by the
// user via `hcloud context create`). Hetzner references keys/servers/firewalls
// by name, which keeps the commands simple.

const (
	hetznerCLI = "hcloud"
	// A current shared x86 type available in the EU + Singapore locations
	// (fsn1/nbg1/hel1/sin). Hetzner's lineup changes and no single type covers
	// every location (US ash/hil use the cpx1x line), so pass --size for those.
	hetznerDefaultType  = "cpx22"
	hetznerDefaultImage = "ubuntu-22.04"
	hetznerKeyName      = "nuzur-deploy"
	hetznerSSHReadyWait = 3 * time.Minute
)

type HetznerProvisioner struct{}

func NewHetznerProvisioner() *HetznerProvisioner { return &HetznerProvisioner{} }

func (p *HetznerProvisioner) Provision(ctx context.Context, spec Spec) (Provisioned, error) {
	if err := ensureProviderCLI(ctx, hetznerCLI, []string{"context", "active"},
		"install it from https://github.com/hetznercloud/cli and run `hcloud context create`"); err != nil {
		return Provisioned{}, err
	}
	cfg := spec.ProviderConfig
	if strings.TrimSpace(cfg.Region) == "" {
		return Provisioned{}, fmt.Errorf("--region is required for Hetzner (a location, e.g. nbg1, fsn1, hel1, ash)")
	}
	serverType := firstNonEmptyStr(cfg.Size, hetznerDefaultType)
	image := firstNonEmptyStr(cfg.Image, hetznerDefaultImage)

	keyName, err := p.ensureSSHKey(ctx, spec)
	if err != nil {
		return Provisioned{}, err
	}

	name, err := providerResourceName(spec.Identifier)
	if err != nil {
		return Provisioned{}, err
	}
	// hcloud waits for the create+start actions before returning.
	if _, err := runCLI(ctx, hetznerCLI, "server", "create",
		"--name", name, "--type", serverType, "--image", image,
		"--location", cfg.Region, "--ssh-key", keyName); err != nil {
		if strings.Contains(err.Error(), "unsupported location for server type") {
			return Provisioned{}, fmt.Errorf("server type %q isn't offered in location %q — run `hcloud server-type list` to see valid types per location and pass a supported one via --size: %w", serverType, cfg.Region, err)
		}
		return Provisioned{}, fmt.Errorf("creating Hetzner server: %w", err)
	}
	id, err := runCLI(ctx, hetznerCLI, "server", "describe", name, "-o", "format={{.ID}}")
	if err != nil {
		return Provisioned{}, fmt.Errorf("reading Hetzner server id: %w", err)
	}
	ip, err := runCLI(ctx, hetznerCLI, "server", "ip", name)
	if err != nil {
		return Provisioned{}, fmt.Errorf("reading Hetzner server ip: %w", err)
	}
	ip = strings.TrimSpace(ip)
	if ip == "" {
		return Provisioned{}, fmt.Errorf("Hetzner server %s has no public IPv4", name)
	}
	target := Target{Host: ip, User: "root", Port: 22, KeyPath: spec.Target.KeyPath}
	if err := sshReady(ctx, target, hetznerSSHReadyWait); err != nil {
		return Provisioned{}, err
	}
	return Provisioned{Target: target, InstanceID: strings.TrimSpace(id), Region: cfg.Region}, nil
}

// ensureSSHKey returns the name of a Hetzner SSH key to attach. It reuses
// --ssh-key-name if given (must already exist), else uploads the local public
// key under nuzur-deploy (idempotent).
func (p *HetznerProvisioner) ensureSSHKey(ctx context.Context, spec Spec) (string, error) {
	name := strings.TrimSpace(spec.ProviderConfig.SSHKeyName)
	if name != "" {
		if _, err := runCLI(ctx, hetznerCLI, "ssh-key", "describe", name); err != nil {
			return "", fmt.Errorf("no Hetzner SSH key named %q — register it first (hcloud ssh-key create) or omit --ssh-key-name to upload your local key", name)
		}
		return name, nil
	}
	if _, err := runCLI(ctx, hetznerCLI, "ssh-key", "describe", hetznerKeyName); err == nil {
		return hetznerKeyName, nil // already uploaded
	}
	pub, err := resolveSSHPublicKey(spec.Target.KeyPath)
	if err != nil {
		return "", err
	}
	if _, err := runCLI(ctx, hetznerCLI, "ssh-key", "create", "--name", hetznerKeyName, "--public-key", pub); err != nil {
		return "", fmt.Errorf("uploading SSH key to Hetzner: %w", err)
	}
	return hetznerKeyName, nil
}

func (p *HetznerProvisioner) ConfigureFirewall(ctx context.Context, prov Provisioned, rules []FirewallRule) error {
	if len(rules) == 0 || prov.InstanceID == "" {
		return nil
	}
	fwName := "nuzur-fw-" + prov.InstanceID
	if _, err := runCLI(ctx, hetznerCLI, "firewall", "create", "--name", fwName); err != nil {
		if !strings.Contains(err.Error(), "already exists") {
			return fmt.Errorf("creating Hetzner firewall: %w", err)
		}
	}
	for _, r := range rules {
		if r.Port <= 0 {
			continue
		}
		port := strconv.Itoa(r.Port)
		if r.PortEnd > 0 {
			port = fmt.Sprintf("%d-%d", r.Port, r.PortEnd)
		}
		if _, err := runCLI(ctx, hetznerCLI, "firewall", "add-rule", fwName,
			"--direction", "in", "--protocol", "tcp", "--port", port,
			"--source-ips", "0.0.0.0/0", "--source-ips", "::/0"); err != nil {
			if !strings.Contains(err.Error(), "already") {
				return fmt.Errorf("adding Hetzner firewall rule for port %s: %w", port, err)
			}
		}
	}
	// Hetzner allows all outbound by default; only inbound needs restricting.
	if _, err := runCLI(ctx, hetznerCLI, "firewall", "apply-to-resource", fwName,
		"--type", "server", "--server", prov.InstanceID); err != nil {
		if !strings.Contains(err.Error(), "already applied") {
			return fmt.Errorf("applying Hetzner firewall: %w", err)
		}
	}
	return nil
}

func (p *HetznerProvisioner) Destroy(ctx context.Context, prov Provisioned) error {
	if strings.TrimSpace(prov.InstanceID) == "" {
		return nil
	}
	if _, err := runCLI(ctx, hetznerCLI, "server", "delete", prov.InstanceID); err != nil {
		return fmt.Errorf("deleting Hetzner server %s: %w", prov.InstanceID, err)
	}
	// Firewall can only be deleted once detached (the server delete detaches it);
	// best-effort.
	_, _ = runCLI(ctx, hetznerCLI, "firewall", "delete", "nuzur-fw-"+prov.InstanceID)
	return nil
}
