package deploy

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// Vultr adapter — shells out to `vultr-cli` (already authenticated by the user).
//
// Two Vultr-specific shapes drive this file:
//
//  1. vultr-cli has no --format projection (only -o json), so unlike the other
//     adapters this one parses JSON rather than reading a line.
//  2. Vultr assigns the public IP ASYNCHRONOUSLY — main_ip is "0.0.0.0" for the
//     first few seconds after create — so the IP is polled rather than read once.
//
// NOTE: the argv here is verified against a real vultr-cli (its own --help
// examples are stale — e.g. they show `firewall rule create --id=<g>`, which the
// binary rejects as an unknown flag; the group id is positional). Not yet run
// against a live account, though: that needs VULTR_API_KEY.

const (
	vultrCLI = "vultr-cli"
	// vc2-1c-2gb is 1 vCPU / 2GB. The generated Dockerfile compiles Go on the box,
	// which OOMs on the 1GB plan.
	vultrDefaultPlan = "vc2-1c-2gb"
	// vultrDefaultOS is Vultr's numeric OS id for Ubuntu 22.04 x64. Vultr addresses
	// images by id, not name — run `vultr-cli os list` if this ever drifts, and
	// pass the id via --image.
	vultrDefaultOS    = "1743"
	vultrKeyName      = "nuzur-deploy"
	vultrSSHReadyWait = 3 * time.Minute
	vultrIPWait       = 2 * time.Minute
)

// vultrIPPoll paces the main_ip wait (var so tests don't sleep).
var vultrIPPoll = 5 * time.Second

type VultrProvisioner struct{}

func NewVultrProvisioner() *VultrProvisioner { return &VultrProvisioner{} }

// vultrInstance is the subset of Vultr's instance object we need. The CLI wraps
// it under "instance"; parsing accepts either shape.
type vultrInstance struct {
	ID     string `json:"id"`
	MainIP string `json:"main_ip"`
}

type vultrInstanceEnvelope struct {
	Instance vultrInstance `json:"instance"`
}

func parseVultrInstance(out string) (vultrInstance, error) {
	var env vultrInstanceEnvelope
	if err := json.Unmarshal([]byte(out), &env); err == nil && env.Instance.ID != "" {
		return env.Instance, nil
	}
	var bare vultrInstance
	if err := json.Unmarshal([]byte(out), &bare); err == nil && bare.ID != "" {
		return bare, nil
	}
	return vultrInstance{}, fmt.Errorf("could not parse the instance from vultr-cli output: %q", out)
}

func (p *VultrProvisioner) Provision(ctx context.Context, spec Spec) (Provisioned, error) {
	if err := ensureProviderCLI(ctx, vultrCLI, []string{"account", "info"},
		"install it from https://github.com/vultr/vultr-cli and export VULTR_API_KEY (or set api-key in its config)"); err != nil {
		return Provisioned{}, err
	}
	cfg := spec.ProviderConfig
	if strings.TrimSpace(cfg.Region) == "" {
		return Provisioned{}, fmt.Errorf("--region is required for Vultr (e.g. ewr, fra) — run `vultr-cli regions list` to see them")
	}
	plan := firstNonEmptyStr(cfg.Size, vultrDefaultPlan)
	os := firstNonEmptyStr(cfg.Image, vultrDefaultOS)

	keyID, err := p.ensureSSHKey(ctx, spec)
	if err != nil {
		return Provisioned{}, err
	}

	name, err := providerResourceName(spec.Identifier)
	if err != nil {
		return Provisioned{}, err
	}
	out, err := runCLI(ctx, vultrCLI, "instance", "create",
		"--region", cfg.Region, "--plan", plan, "--os", os,
		"--host", name, "--label", name,
		"--ssh-keys", keyID, "-o", "json")
	if err != nil {
		if strings.Contains(err.Error(), "os") || strings.Contains(err.Error(), "image") {
			return Provisioned{}, fmt.Errorf("creating Vultr instance (Vultr addresses images by numeric id — run `vultr-cli os list` and pass it via --image; the default is %s = Ubuntu 22.04): %w", vultrDefaultOS, err)
		}
		return Provisioned{}, fmt.Errorf("creating Vultr instance: %w", err)
	}
	inst, err := parseVultrInstance(out)
	if err != nil {
		return Provisioned{}, err
	}

	ip, err := p.waitForIP(ctx, inst)
	if err != nil {
		return Provisioned{}, err
	}
	target := Target{Host: ip, User: "root", Port: 22, KeyPath: spec.Target.KeyPath}
	if err := sshReady(ctx, target, vultrSSHReadyWait); err != nil {
		return Provisioned{}, err
	}
	return Provisioned{Target: target, InstanceID: inst.ID, Region: cfg.Region}, nil
}

// waitForIP polls until Vultr has assigned a real public IP. Fresh instances
// report "0.0.0.0" for a few seconds; using that as the host would make the SSH
// wait fail for the wrong reason.
func (p *VultrProvisioner) waitForIP(ctx context.Context, inst vultrInstance) (string, error) {
	if vultrIPAssigned(inst.MainIP) {
		return inst.MainIP, nil
	}
	deadline := time.Now().Add(vultrIPWait)
	for {
		out, err := runCLI(ctx, vultrCLI, "instance", "get", inst.ID, "-o", "json")
		if err == nil {
			if got, perr := parseVultrInstance(out); perr == nil && vultrIPAssigned(got.MainIP) {
				return got.MainIP, nil
			}
		}
		if time.Now().After(deadline) {
			return "", fmt.Errorf("timed out waiting for Vultr to assign a public IP to instance %s", inst.ID)
		}
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case <-time.After(vultrIPPoll):
		}
	}
}

func vultrIPAssigned(ip string) bool {
	ip = strings.TrimSpace(ip)
	return ip != "" && ip != "0.0.0.0"
}

// ensureSSHKey returns a Vultr ssh-key id: --ssh-key-name if it resolves, else
// the nuzur-deploy key, uploading the local public key if it isn't there yet.
func (p *VultrProvisioner) ensureSSHKey(ctx context.Context, spec Spec) (string, error) {
	name := strings.TrimSpace(spec.ProviderConfig.SSHKeyName)
	if id := p.findSSHKeyID(ctx, name); id != "" {
		return id, nil
	}
	if name != "" {
		// Not found by name — assume the user passed an id.
		return name, nil
	}
	pub, err := resolveSSHPublicKey(spec.Target.KeyPath)
	if err != nil {
		return "", err
	}
	out, err := runCLI(ctx, vultrCLI, "ssh-key", "create",
		"--name", vultrKeyName, "--key", pub, "-o", "json")
	if err != nil {
		return "", fmt.Errorf("uploading SSH key to Vultr: %w", err)
	}
	var env struct {
		SSHKey struct {
			ID string `json:"id"`
		} `json:"ssh_key"`
	}
	if err := json.Unmarshal([]byte(out), &env); err != nil || env.SSHKey.ID == "" {
		return "", fmt.Errorf("could not parse the ssh-key id from vultr-cli output: %q", out)
	}
	return env.SSHKey.ID, nil
}

// findSSHKeyID resolves a Vultr ssh-key name to its id, or "". An empty name
// looks up the default nuzur-deploy key.
func (p *VultrProvisioner) findSSHKeyID(ctx context.Context, name string) string {
	want := name
	if want == "" {
		want = vultrKeyName
	}
	out, err := runCLI(ctx, vultrCLI, "ssh-key", "list", "-o", "json")
	if err != nil {
		return ""
	}
	var env struct {
		SSHKeys []struct {
			ID   string `json:"id"`
			Name string `json:"name"`
		} `json:"ssh_keys"`
	}
	if err := json.Unmarshal([]byte(out), &env); err != nil {
		return ""
	}
	for _, k := range env.SSHKeys {
		if k.Name == want {
			return k.ID
		}
	}
	return ""
}

func (p *VultrProvisioner) ConfigureFirewall(ctx context.Context, prov Provisioned, rules []FirewallRule) error {
	if strings.TrimSpace(prov.InstanceID) == "" || len(rules) == 0 {
		return nil
	}
	desc := vultrFirewallName(prov.InstanceID)
	groupID := p.findFirewallGroupID(ctx, desc)
	if groupID == "" {
		out, err := runCLI(ctx, vultrCLI, "firewall", "group", "create", "--description", desc, "-o", "json")
		if err != nil {
			return fmt.Errorf("creating Vultr firewall group: %w", err)
		}
		var env struct {
			FirewallGroup struct {
				ID string `json:"id"`
			} `json:"firewall_group"`
		}
		if err := json.Unmarshal([]byte(out), &env); err != nil || env.FirewallGroup.ID == "" {
			return fmt.Errorf("could not parse the firewall group id from vultr-cli output: %q", out)
		}
		groupID = env.FirewallGroup.ID
	}

	for _, r := range rules {
		if r.Port <= 0 {
			continue
		}
		port := strconv.Itoa(r.Port)
		if r.PortEnd > 0 {
			// Vultr expresses ranges with a colon.
			port = fmt.Sprintf("%d:%d", r.Port, r.PortEnd)
		}
		// The group id is positional; --ip-type is required. `--subnet 0.0.0.0
		// --size 0` is Vultr's spelling of "any source" (size is the CIDR prefix).
		_, err := runCLI(ctx, vultrCLI, "firewall", "rule", "create", groupID,
			"--ip-type", "v4", "--protocol", "tcp",
			"--subnet", "0.0.0.0", "--size", "0", "--port", port)
		if err != nil && !strings.Contains(err.Error(), "already") {
			return fmt.Errorf("creating Vultr firewall rule for port %s: %w", port, err)
		}
	}

	// Attach the group to the instance (its own subcommand — `instance update`
	// has no firewall flag).
	if _, err := runCLI(ctx, vultrCLI, "instance", "update-firewall-group", prov.InstanceID,
		"--firewall-group-id", groupID); err != nil {
		return fmt.Errorf("attaching Vultr firewall group to instance %s: %w", prov.InstanceID, err)
	}
	return nil
}

func (p *VultrProvisioner) Destroy(ctx context.Context, prov Provisioned) error {
	if strings.TrimSpace(prov.InstanceID) == "" {
		return nil
	}
	if _, err := runCLI(ctx, vultrCLI, "instance", "delete", prov.InstanceID); err != nil {
		return fmt.Errorf("deleting Vultr instance %s: %w", prov.InstanceID, err)
	}
	// The group can only go once the instance releases it.
	if gid := p.findFirewallGroupID(ctx, vultrFirewallName(prov.InstanceID)); gid != "" {
		_, _ = runCLI(ctx, vultrCLI, "firewall", "group", "delete", gid)
	}
	return nil
}

func (p *VultrProvisioner) findFirewallGroupID(ctx context.Context, desc string) string {
	out, err := runCLI(ctx, vultrCLI, "firewall", "group", "list", "-o", "json")
	if err != nil {
		return ""
	}
	var env struct {
		FirewallGroups []struct {
			ID          string `json:"id"`
			Description string `json:"description"`
		} `json:"firewall_groups"`
	}
	if err := json.Unmarshal([]byte(out), &env); err != nil {
		return ""
	}
	for _, g := range env.FirewallGroups {
		if g.Description == desc {
			return g.ID
		}
	}
	return ""
}

func vultrFirewallName(instanceID string) string { return "nuzur-fw-" + instanceID }
