package deploy

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// Scaleway adapter — shells out to `scw` (already authenticated by the user).
//
// Scaleway-specific shapes:
//
//  1. SSH KEYS ARE ACCOUNT-LEVEL. Scaleway injects every SSH key registered on
//     the project into new servers automatically, so — unlike DigitalOcean/Vultr —
//     there is no per-deploy key to upload or reference. The adapter only checks
//     that at least one key exists, because a server with none is unreachable.
//  2. `scw` takes key=value arguments (not --flags) and can project output with
//     `-o template=`, which keeps parsing line-oriented like the other adapters.
//
// NOTE: the argv here is verified against a real scw binary (notably SSH keys
// live under `iam`, not `account` — `scw account ssh-key list` silently prints
// help rather than erroring). Not yet run against a live account, though: that
// needs `scw init`.

const (
	scalewayCLI = "scw"
	// DEV1-S is 2 vCPU / 2GB. The generated Dockerfile compiles Go on the box,
	// which OOMs on 1GB shapes.
	scalewayDefaultType = "DEV1-S"
	// Default ZONE — --region is optional and carries a zone for Scaleway.
	scalewayDefaultZone  = "fr-par-1"
	scalewayDefaultImage = "ubuntu_jammy"
	scalewaySSHReadyWait = 3 * time.Minute
)

type ScalewayProvisioner struct{}

func NewScalewayProvisioner() *ScalewayProvisioner { return &ScalewayProvisioner{} }

func (p *ScalewayProvisioner) Provision(ctx context.Context, spec Spec) (Provisioned, error) {
	if err := ensureProviderCLI(ctx, scalewayCLI, []string{"config", "get", "default-project-id"},
		"install it from https://github.com/scaleway/scaleway-cli and run `scw init`"); err != nil {
		return Provisioned{}, err
	}
	cfg := spec.ProviderConfig
	// NB: for Scaleway --region carries a ZONE (fr-par-1), not a region.
	zone := firstNonEmptyStr(cfg.Region, scalewayDefaultZone)
	serverType := firstNonEmptyStr(cfg.Size, scalewayDefaultType)
	image := firstNonEmptyStr(cfg.Image, scalewayDefaultImage)

	// Scaleway injects the account's SSH keys at boot; with none registered the
	// server would come up unreachable, so fail early and actionably instead.
	if err := p.ensureAccountSSHKey(ctx); err != nil {
		return Provisioned{}, err
	}

	name, err := providerResourceName(spec.Identifier)
	if err != nil {
		return Provisioned{}, err
	}
	out, err := runCLI(ctx, scalewayCLI, "instance", "server", "create",
		"name="+name, "type="+serverType, "image="+image,
		"zone="+zone, "ip=new",
		"-o", "template={{.ID}} {{.PublicIP.Address}}")
	if err != nil {
		return Provisioned{}, fmt.Errorf("creating Scaleway server: %w", err)
	}
	fields := strings.Fields(out)
	if len(fields) < 2 || fields[0] == "" || fields[1] == "" {
		return Provisioned{}, fmt.Errorf("could not parse server id/ip from scw output: %q", out)
	}
	target := Target{Host: fields[1], User: "root", Port: 22, KeyPath: spec.Target.KeyPath}
	if err := sshReady(ctx, target, scalewaySSHReadyWait); err != nil {
		return Provisioned{}, err
	}
	return Provisioned{Target: target, InstanceID: fields[0], Region: zone}, nil
}

// ensureAccountSSHKey verifies the account has at least one SSH key, since that's
// the only way a Scaleway server gets one.
func (p *ScalewayProvisioner) ensureAccountSSHKey(ctx context.Context) error {
	out, err := runCLI(ctx, scalewayCLI, "iam", "ssh-key", "list", "-o", "template={{.ID}}")
	if err != nil {
		return fmt.Errorf("listing Scaleway SSH keys: %w", err)
	}
	if strings.TrimSpace(out) == "" {
		return fmt.Errorf("no SSH key is registered on your Scaleway account — Scaleway injects account keys into new servers, so the box would be unreachable. Add one with `scw iam ssh-key create name=nuzur public-key=\"$(cat ~/.ssh/id_ed25519.pub)\"`")
	}
	return nil
}

func (p *ScalewayProvisioner) ConfigureFirewall(ctx context.Context, prov Provisioned, rules []FirewallRule) error {
	if strings.TrimSpace(prov.InstanceID) == "" || len(rules) == 0 {
		return nil
	}
	zone := prov.Region
	name := scalewayFirewallName(prov.InstanceID)

	// A security group that drops inbound by default; each rule opens one port.
	sgID, err := runCLI(ctx, scalewayCLI, "instance", "security-group", "create",
		"name="+name, "zone="+zone,
		"inbound-default-policy=drop", "outbound-default-policy=accept",
		"-o", "template={{.ID}}")
	if err != nil {
		if !strings.Contains(err.Error(), "already") {
			return fmt.Errorf("creating Scaleway security group: %w", err)
		}
		sgID = p.findSecurityGroupID(ctx, zone, name)
	}
	sgID = strings.TrimSpace(sgID)
	if sgID == "" {
		return fmt.Errorf("could not resolve the Scaleway security group id for %q", name)
	}

	for _, r := range rules {
		if r.Port <= 0 {
			continue
		}
		from := strconv.Itoa(r.Port)
		to := from
		if r.PortEnd > 0 {
			to = strconv.Itoa(r.PortEnd)
		}
		_, err := runCLI(ctx, scalewayCLI, "instance", "security-group", "create-rule",
			"security-group-id="+sgID, "zone="+zone,
			"direction=inbound", "action=accept", "protocol=TCP",
			"ip-range=0.0.0.0/0",
			"dest-port-from="+from, "dest-port-to="+to)
		if err != nil && !strings.Contains(err.Error(), "already") {
			return fmt.Errorf("creating Scaleway security-group rule for port %s: %w", from, err)
		}
	}

	// Attach the group to the server.
	if _, err := runCLI(ctx, scalewayCLI, "instance", "server", "update", prov.InstanceID,
		"zone="+zone, "security-group-id="+sgID); err != nil {
		return fmt.Errorf("attaching Scaleway security group to server %s: %w", prov.InstanceID, err)
	}
	return nil
}

func (p *ScalewayProvisioner) Destroy(ctx context.Context, prov Provisioned) error {
	if strings.TrimSpace(prov.InstanceID) == "" {
		return nil
	}
	// with-ip/with-block free the public IP and volumes too — leaving them behind
	// keeps billing after the server is gone.
	if _, err := runCLI(ctx, scalewayCLI, "instance", "server", "terminate", prov.InstanceID,
		"zone="+prov.Region, "with-ip=true", "with-block=true"); err != nil {
		return fmt.Errorf("terminating Scaleway server %s: %w", prov.InstanceID, err)
	}
	if sgID := p.findSecurityGroupID(ctx, prov.Region, scalewayFirewallName(prov.InstanceID)); sgID != "" {
		_, _ = runCLI(ctx, scalewayCLI, "instance", "security-group", "delete", sgID, "zone="+prov.Region)
	}
	return nil
}

// findSecurityGroupID resolves a security-group name to its id, or "".
func (p *ScalewayProvisioner) findSecurityGroupID(ctx context.Context, zone, name string) string {
	out, err := runCLI(ctx, scalewayCLI, "instance", "security-group", "list",
		"zone="+zone, "-o", "template={{.ID}} {{.Name}}")
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

func scalewayFirewallName(instanceID string) string { return "nuzur-fw-" + instanceID }
