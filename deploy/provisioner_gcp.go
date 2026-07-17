package deploy

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// GCP adapter — shells out to `gcloud` (already authenticated by the user, with a
// default project configured). Creates a Compute Engine instance, opens inbound
// with a tag-targeted firewall rule, and deletes both on destroy.
//
// Two GCP-specific shapes drive this file:
//
//  1. GCP firewall rules are NETWORK-level and select instances by NETWORK TAG —
//     there is no per-instance firewall object like DigitalOcean's or Hetzner's.
//     So the instance is tagged at create and the rule targets that tag.
//  2. ConfigureFirewall/Destroy only receive Provisioned, which carries no
//     instance name — but the tag IS the name. So InstanceID holds the instance
//     NAME rather than the numeric id (gcloud addresses instances by name+zone
//     anyway), and Region carries the zone.

const (
	gcpCLI = "gcloud"
	// e2-small is 2GB: the generated Dockerfile compiles Go on the box, which OOMs
	// on 1GB shapes like e2-micro.
	gcpDefaultMachineType = "e2-small"
	// Default ZONE — --region is optional and carries a zone for GCP.
	gcpDefaultZone = "us-central1-a"
	// Ubuntu 24.04 LTS (noble) — see doDefaultImage. UNVERIFIED against a live
	// `gcloud compute images list --project ubuntu-os-cloud` (needs auth); it follows
	// the family naming of the 2204 value it replaces. Confirm at the first GCP E2E.
	gcpDefaultImageFamily = "ubuntu-2404-lts"
	gcpImageProject       = "ubuntu-os-cloud"
	gcpSSHReadyWait       = 3 * time.Minute
)

type GCPProvisioner struct{}

func NewGCPProvisioner() *GCPProvisioner { return &GCPProvisioner{} }

func (p *GCPProvisioner) Provision(ctx context.Context, spec Spec) (Provisioned, error) {
	if err := ensureProviderCLI(ctx, gcpCLI, []string{"config", "get-value", "project"},
		"install it from https://cloud.google.com/sdk/docs/install and run `gcloud auth login`"); err != nil {
		return Provisioned{}, err
	}
	// gcloud needs a project; we use the CLI's configured default rather than
	// asking for one, so nuzur never holds provider config. An unset project
	// otherwise fails deep inside the create with a confusing message.
	if err := p.ensureProject(ctx); err != nil {
		return Provisioned{}, err
	}
	cfg := spec.ProviderConfig
	// NB: for GCP --region carries a ZONE (us-central1-a), not a region.
	zone := firstNonEmptyStr(cfg.Region, gcpDefaultZone)
	machineType := firstNonEmptyStr(cfg.Size, gcpDefaultMachineType)
	imageFamily := firstNonEmptyStr(cfg.Image, gcpDefaultImageFamily)

	pub, err := resolveSSHPublicKey(spec.Target.KeyPath)
	if err != nil {
		return Provisioned{}, err
	}

	// The name doubles as the network tag the firewall rule targets.
	name, err := specResourceName(spec)
	if err != nil {
		return Provisioned{}, err
	}
	out, err := runCLI(ctx, gcpCLI, "compute", "instances", "create", name,
		"--zone", zone,
		"--machine-type", machineType,
		"--image-family", imageFamily,
		"--image-project", gcpImageProject,
		"--tags", name,
		"--metadata", "ssh-keys=root:"+pub,
		"--format", "value(name,networkInterfaces[0].accessConfigs[0].natIP)")
	if err != nil {
		return Provisioned{}, fmt.Errorf("creating GCP instance: %w", err)
	}
	fields := strings.Fields(out)
	if len(fields) < 2 || fields[0] == "" || fields[1] == "" {
		return Provisioned{}, fmt.Errorf("could not parse instance name/ip from gcloud output: %q", out)
	}
	target := Target{Host: fields[1], User: "root", Port: 22, KeyPath: spec.Target.KeyPath}
	// The instance exists and is billing — report it before the SSH wait below.
	// InstanceID is the instance NAME — see the file comment.
	reportInstance(spec, InstanceRef{
		InstanceID: fields[0], Region: zone, Host: fields[1], ResourceName: name,
	})
	if err := sshReady(ctx, target, gcpSSHReadyWait); err != nil {
		return Provisioned{}, err
	}
	// InstanceID is the instance NAME — see the file comment.
	return Provisioned{Target: target, InstanceID: fields[0], Region: zone}, nil
}

// FindInstanceByName resolves a GCP instance name — which for this adapter IS the
// instance id (see the file comment) — confirming it exists in the zone. gcloud
// exits 0 with no output when the filter matches nothing.
func (p *GCPProvisioner) FindInstanceByName(ctx context.Context, name, region string) (string, error) {
	if strings.TrimSpace(name) == "" {
		return "", nil
	}
	zone := firstNonEmptyStr(region, gcpDefaultZone)
	out, err := runCLI(ctx, gcpCLI, "compute", "instances", "list",
		"--filter", "name="+name, "--zones", zone, "--format", "value(name)")
	if err != nil {
		return "", fmt.Errorf("listing GCP instances to find %q: %w", name, err)
	}
	for _, line := range strings.Split(out, "\n") {
		if strings.TrimSpace(line) == name {
			return name, nil
		}
	}
	return "", nil
}

// ensureProject fails early and actionably when gcloud has no default project.
func (p *GCPProvisioner) ensureProject(ctx context.Context) error {
	out, err := runCLI(ctx, gcpCLI, "config", "get-value", "project")
	project := strings.TrimSpace(out)
	if err != nil || project == "" || project == "(unset)" {
		return fmt.Errorf("gcloud has no default project set — run `gcloud config set project <project-id>` (nuzur uses your gcloud configuration rather than storing provider settings)")
	}
	return nil
}

func (p *GCPProvisioner) ConfigureFirewall(ctx context.Context, prov Provisioned, rules []FirewallRule) error {
	name := strings.TrimSpace(prov.InstanceID) // the instance name == the network tag
	if name == "" {
		return nil
	}
	allow := gcpAllowRules(rules)
	if allow == "" {
		return nil
	}
	// One network-level rule targeting only this instance's tag.
	_, err := runCLI(ctx, gcpCLI, "compute", "firewall-rules", "create", gcpFirewallName(name),
		"--allow", allow,
		"--target-tags", name,
		"--source-ranges", "0.0.0.0/0",
		"--direction", "INGRESS")
	if err != nil && !strings.Contains(err.Error(), "already exists") {
		return fmt.Errorf("creating GCP firewall rule: %w", err)
	}
	return nil
}

func (p *GCPProvisioner) Destroy(ctx context.Context, prov Provisioned) error {
	name := strings.TrimSpace(prov.InstanceID)
	if name == "" {
		return nil
	}
	_, _ = runCLI(ctx, gcpCLI, "compute", "firewall-rules", "delete", gcpFirewallName(name), "--quiet")
	if _, err := runCLI(ctx, gcpCLI, "compute", "instances", "delete", name,
		"--zone", prov.Region, "--quiet"); err != nil {
		return fmt.Errorf("deleting GCP instance %s: %w", name, err)
	}
	return nil
}

func gcpFirewallName(instanceName string) string { return "nuzur-fw-" + instanceName }

// gcpAllowRules renders FirewallRules as gcloud's --allow list, e.g.
// "tcp:22,tcp:80,tcp:8443-8542". Sources are unrestricted; the box's ufw is the
// precise gate.
func gcpAllowRules(rules []FirewallRule) string {
	var parts []string
	for _, r := range rules {
		if r.Port <= 0 {
			continue
		}
		ports := strconv.Itoa(r.Port)
		if r.PortEnd > 0 {
			ports = fmt.Sprintf("%d-%d", r.Port, r.PortEnd)
		}
		parts = append(parts, "tcp:"+ports)
	}
	return strings.Join(parts, ",")
}
