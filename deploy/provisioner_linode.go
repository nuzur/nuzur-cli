package deploy

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// Linode (Akamai) adapter — shells out to `linode-cli` (already authenticated by
// the user). Creates a linode, restricts inbound with a cloud firewall, and
// deletes both on destroy.
//
// Simpler than DigitalOcean in one respect: the public key is passed INLINE at
// create (--authorized_keys), so there's no key upload/lookup step.

const (
	linodeCLI = "linode-cli"
	// Linode 2GB. The generated Dockerfile compiles Go on the box, which OOMs on
	// the 1GB nanode — so the default is the smallest tier that reliably builds.
	linodeDefaultType = "g6-standard-1"
	// Default region — --region is optional.
	linodeDefaultRegion = "us-east"
	linodeDefaultImage  = "linode/ubuntu24.04" // Ubuntu 24.04 LTS (noble) — see doDefaultImage
	linodeSSHReadyWait  = 3 * time.Minute
)

type LinodeProvisioner struct{}

func NewLinodeProvisioner() *LinodeProvisioner { return &LinodeProvisioner{} }

func (p *LinodeProvisioner) Provision(ctx context.Context, spec Spec) (Provisioned, error) {
	if err := ensureProviderCLI(ctx, linodeCLI, []string{"account", "view", "--text", "--no-headers"},
		"install it from https://techdocs.akamai.com/cloud-computing/docs/install-and-configure-the-cli and run `linode-cli configure`"); err != nil {
		return Provisioned{}, err
	}
	cfg := spec.ProviderConfig
	region := firstNonEmptyStr(cfg.Region, linodeDefaultRegion)
	linodeType := firstNonEmptyStr(cfg.Size, linodeDefaultType)
	image := firstNonEmptyStr(cfg.Image, linodeDefaultImage)

	// Linode takes the public key inline; no key is registered on the account.
	pub, err := resolveSSHPublicKey(spec.Target.KeyPath)
	if err != nil {
		return Provisioned{}, err
	}
	rootPass, err := linodeRootPassword()
	if err != nil {
		return Provisioned{}, err
	}

	name, err := specResourceName(spec)
	if err != nil {
		return Provisioned{}, err
	}
	out, err := runCLI(ctx, linodeCLI, "linodes", "create",
		"--label", name, "--region", region,
		"--type", linodeType, "--image", image,
		"--authorized_keys", pub, "--root_pass", rootPass,
		"--text", "--no-headers", "--format", "id,ipv4")
	if err != nil {
		if strings.Contains(err.Error(), "not valid") || strings.Contains(err.Error(), "type") && strings.Contains(err.Error(), "region") {
			return Provisioned{}, fmt.Errorf("creating linode (check that type %q is offered in region %q — `linode-cli linodes types` / `linode-cli regions list`): %w", linodeType, region, err)
		}
		return Provisioned{}, fmt.Errorf("creating linode: %w", err)
	}
	fields := strings.Fields(out)
	if len(fields) < 2 || fields[0] == "" || fields[1] == "" {
		return Provisioned{}, fmt.Errorf("could not parse linode id/ip from linode-cli output: %q", out)
	}
	target := Target{Host: fields[1], User: "root", Port: 22, KeyPath: spec.Target.KeyPath}
	// The linode exists and is billing from here on. Report it BEFORE the SSH wait
	// below, which can take minutes — dying in that gap used to strand the VM with
	// nothing on disk pointing at it.
	reportInstance(spec, InstanceRef{
		InstanceID: fields[0], Region: region, Host: fields[1], ResourceName: name,
	})
	if err := sshReady(ctx, target, linodeSSHReadyWait); err != nil {
		return Provisioned{}, err
	}
	return Provisioned{Target: target, InstanceID: fields[0], Region: region}, nil
}

// linodeRootPassword generates a throwaway root password. Linode requires one at
// create even for a key-only login; nothing ever uses it (the bootstrap logs in
// with the SSH key), so it's generated, sent once, and forgotten.
func linodeRootPassword() (string, error) {
	b := make([]byte, 24)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("generating linode root password: %w", err)
	}
	// Mixed case + digits + punctuation satisfies Linode's strength requirement.
	return "Nz1-" + base64.RawURLEncoding.EncodeToString(b), nil
}

func (p *LinodeProvisioner) ConfigureFirewall(ctx context.Context, prov Provisioned, rules []FirewallRule) error {
	if strings.TrimSpace(prov.InstanceID) == "" {
		return nil
	}
	inbound, err := linodeInboundRules(rules)
	if err != nil {
		return err
	}
	if inbound == "" {
		return nil
	}
	label := linodeFirewallLabel(prov.InstanceID)

	fwID, err := runCLI(ctx, linodeCLI, "firewalls", "create",
		"--label", label,
		"--rules.inbound_policy", "DROP",
		"--rules.outbound_policy", "ACCEPT",
		"--rules.inbound", inbound,
		"--text", "--no-headers", "--format", "id")
	if err != nil {
		if !strings.Contains(err.Error(), "already") {
			return fmt.Errorf("creating Linode firewall: %w", err)
		}
		// Re-deploy against the same instance: reuse the existing firewall.
		fwID = p.findFirewallID(ctx, label)
	}
	fwID = strings.TrimSpace(fwID)
	if fwID == "" {
		return fmt.Errorf("could not resolve the Linode firewall id for %q", label)
	}

	// Attach the firewall to the linode.
	if _, err := runCLI(ctx, linodeCLI, "firewalls", "device-create", fwID,
		"--id", prov.InstanceID, "--type", "linode"); err != nil {
		if !strings.Contains(err.Error(), "already") {
			return fmt.Errorf("attaching Linode firewall to linode %s: %w", prov.InstanceID, err)
		}
	}
	return nil
}

func (p *LinodeProvisioner) Destroy(ctx context.Context, prov Provisioned) error {
	if strings.TrimSpace(prov.InstanceID) == "" {
		return nil
	}
	if fwID := p.findFirewallID(ctx, linodeFirewallLabel(prov.InstanceID)); fwID != "" {
		_, _ = runCLI(ctx, linodeCLI, "firewalls", "delete", fwID)
	}
	if _, err := runCLI(ctx, linodeCLI, "linodes", "delete", prov.InstanceID); err != nil {
		return fmt.Errorf("deleting linode %s: %w", prov.InstanceID, err)
	}
	return nil
}

// FindInstanceByName resolves a linode label to its id. Linode filters server-side
// on --label and exits 0 with no output when nothing matches, so an empty result is
// "no such linode", not an error.
func (p *LinodeProvisioner) FindInstanceByName(ctx context.Context, name, region string) (string, error) {
	if strings.TrimSpace(name) == "" {
		return "", nil
	}
	out, err := runCLI(ctx, linodeCLI, "linodes", "list",
		"--label", name, "--text", "--no-headers", "--format", "id,label")
	if err != nil {
		return "", fmt.Errorf("looking up linode %q: %w", name, err)
	}
	for _, line := range strings.Split(out, "\n") {
		f := strings.Fields(line)
		// Match the label exactly: --label is a filter, but never delete something
		// whose name isn't the one we minted.
		if len(f) >= 2 && f[1] == name {
			return f[0], nil
		}
	}
	return "", nil
}

// findFirewallID resolves a firewall label to its id, or "" if not found.
func (p *LinodeProvisioner) findFirewallID(ctx context.Context, label string) string {
	out, err := runCLI(ctx, linodeCLI, "firewalls", "list", "--text", "--no-headers", "--format", "id,label")
	if err != nil {
		return ""
	}
	for _, line := range strings.Split(out, "\n") {
		f := strings.Fields(line)
		if len(f) >= 2 && f[1] == label {
			return f[0]
		}
	}
	return ""
}

// linodeFirewallLabel names this deployment's firewall. Linode labels allow
// alphanumerics, dashes and underscores (3–32 chars), which the instance id
// always satisfies.
func linodeFirewallLabel(instanceID string) string { return "nuzur-fw-" + instanceID }

// Linode's firewall rules are a JSON array (the one provider where the CLI takes
// JSON rather than a flag-shaped rule), so they're marshaled rather than
// hand-formatted.
type linodeFWAddresses struct {
	IPv4 []string `json:"ipv4"`
	IPv6 []string `json:"ipv6"`
}

type linodeFWRule struct {
	Protocol  string            `json:"protocol"`
	Ports     string            `json:"ports"`
	Addresses linodeFWAddresses `json:"addresses"`
	Action    string            `json:"action"`
	Label     string            `json:"label"`
}

// linodeInboundRules renders FirewallRules as Linode's inbound JSON array (all
// sources allowed; the on-box ufw is the precise gate). Returns "" for no rules.
func linodeInboundRules(rules []FirewallRule) (string, error) {
	out := []linodeFWRule{}
	for _, r := range rules {
		if r.Port <= 0 {
			continue
		}
		ports := strconv.Itoa(r.Port)
		label := "nuzur-tcp-" + ports
		if r.PortEnd > 0 {
			ports = fmt.Sprintf("%d-%d", r.Port, r.PortEnd)
			label = fmt.Sprintf("nuzur-tcp-%d-%d", r.Port, r.PortEnd)
		}
		out = append(out, linodeFWRule{
			Protocol:  "TCP",
			Ports:     ports,
			Addresses: linodeFWAddresses{IPv4: []string{"0.0.0.0/0"}, IPv6: []string{"::/0"}},
			Action:    "ACCEPT",
			Label:     label,
		})
	}
	if len(out) == 0 {
		return "", nil
	}
	b, err := json.Marshal(out)
	if err != nil {
		return "", fmt.Errorf("building Linode inbound rules: %w", err)
	}
	return string(b), nil
}
