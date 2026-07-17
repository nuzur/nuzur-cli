package deploy

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// Azure adapter — shells out to `az` (already authenticated by the user). Creates
// a resource group, a VM inside it, opens inbound ports on the VM's NSG, and
// deletes the whole resource group on destroy.
//
// Two Azure-specific shapes drive this file:
//
//  1. RESOURCE GROUP PER DEPLOYMENT. `az vm create` implicitly creates a vnet,
//     NIC, NSG, public IP and disk; deleting just the VM would orphan all of them
//     (and keep billing). Creating everything inside a dedicated group makes
//     Destroy a single `az group delete`, which reaps the lot. So InstanceID holds
//     the RESOURCE GROUP name.
//  2. AZURE FORBIDS `root` AS THE ADMIN USERNAME. This is the only adapter whose
//     Target.User isn't root; the deploy compensates automatically because
//     command_deploy.go sets `runner.Sudo = s.Sudo || target.User != "root"`, and
//     Azure's cloud-init user has passwordless sudo.

const (
	azureCLI = "az"
	// Standard_B2s is 2 vCPU / 4GB. The generated Dockerfile compiles Go on the
	// box; B1s (1GB) OOMs.
	azureDefaultSize  = "Standard_B2s"
	azureDefaultImage = "Ubuntu2204"
	// azureAdminUser is the SSH user. Azure rejects "root" (and a list of other
	// reserved names) as --admin-username.
	azureAdminUser    = "nuzur"
	azureSSHReadyWait = 3 * time.Minute
	// azureFirewallBasePriority is where nuzur's NSG rules start. Azure requires a
	// unique priority per rule (100–4096); `az vm open-port` assigns 900 by
	// default, which collides once we open more than one port.
	azureFirewallBasePriority = 1000
)

type AzureProvisioner struct{}

func NewAzureProvisioner() *AzureProvisioner { return &AzureProvisioner{} }

func (p *AzureProvisioner) Provision(ctx context.Context, spec Spec) (Provisioned, error) {
	if err := ensureProviderCLI(ctx, azureCLI, []string{"account", "show"},
		"install it from https://learn.microsoft.com/cli/azure/install-azure-cli and run `az login`"); err != nil {
		return Provisioned{}, err
	}
	cfg := spec.ProviderConfig
	if strings.TrimSpace(cfg.Region) == "" {
		return Provisioned{}, fmt.Errorf("--region is required for Azure (e.g. eastus, westeurope) — run `az account list-locations --query \"[].name\" -o tsv` to see them")
	}
	size := firstNonEmptyStr(cfg.Size, azureDefaultSize)
	image := firstNonEmptyStr(cfg.Image, azureDefaultImage)

	pub, err := resolveSSHPublicKey(spec.Target.KeyPath)
	if err != nil {
		return Provisioned{}, err
	}

	name, err := providerResourceName(spec.Identifier)
	if err != nil {
		return Provisioned{}, err
	}
	// Refuse to touch a group we didn't just name. `az group create` is an
	// idempotent ARM PUT: on an existing group it SUCCEEDS and returns it — so
	// without this guard a name collision would silently adopt the user's group,
	// and Destroy (which deletes the whole group) would take their resources with
	// it. providerResourceName's random suffix already makes this near-impossible;
	// this is the belt to that braces, because the failure mode is data loss.
	if exists, _ := runCLI(ctx, azureCLI, "group", "exists", "--name", name); strings.TrimSpace(exists) == "true" {
		return Provisioned{}, fmt.Errorf("azure resource group %q already exists — refusing to deploy into a group nuzur didn't create (destroy deletes the whole group). Re-run to get a fresh name, or delete it first", name)
	}
	// The resource group is the unit of cleanup — see the file comment.
	if _, err := runCLI(ctx, azureCLI, "group", "create",
		"--name", name, "--location", cfg.Region); err != nil {
		return Provisioned{}, fmt.Errorf("creating Azure resource group: %w", err)
	}

	ip, err := runCLI(ctx, azureCLI, "vm", "create",
		"--resource-group", name, "--name", name,
		"--image", image, "--size", size, "--location", cfg.Region,
		"--admin-username", azureAdminUser,
		"--ssh-key-values", pub,
		"--public-ip-sku", "Standard",
		"--query", "publicIpAddress", "-o", "tsv")
	if err != nil {
		// The group is already there; leave it so Destroy can still reap whatever
		// was half-created rather than orphaning it.
		return Provisioned{}, fmt.Errorf("creating Azure VM (the resource group %q was created — `nuzur destroy` or `az group delete -n %s` will clean it up): %w", name, name, err)
	}
	ip = strings.TrimSpace(ip)
	if ip == "" {
		return Provisioned{}, fmt.Errorf("could not parse the public IP from az vm create output")
	}
	target := Target{Host: ip, User: azureAdminUser, Port: 22, KeyPath: spec.Target.KeyPath}
	if err := sshReady(ctx, target, azureSSHReadyWait); err != nil {
		return Provisioned{}, err
	}
	// InstanceID is the RESOURCE GROUP name — see the file comment.
	return Provisioned{Target: target, InstanceID: name, Region: cfg.Region}, nil
}

func (p *AzureProvisioner) ConfigureFirewall(ctx context.Context, prov Provisioned, rules []FirewallRule) error {
	rg := strings.TrimSpace(prov.InstanceID) // resource group == VM name
	if rg == "" {
		return nil
	}
	priority := azureFirewallBasePriority
	for _, r := range rules {
		if r.Port <= 0 {
			continue
		}
		port := strconv.Itoa(r.Port)
		if r.PortEnd > 0 {
			port = fmt.Sprintf("%d-%d", r.Port, r.PortEnd)
		}
		// Each rule needs its own priority, or Azure rejects the duplicate.
		_, err := runCLI(ctx, azureCLI, "vm", "open-port",
			"--resource-group", rg, "--name", rg,
			"--port", port, "--priority", strconv.Itoa(priority))
		if err != nil && !strings.Contains(err.Error(), "already") {
			return fmt.Errorf("opening port %s on the Azure VM: %w", port, err)
		}
		priority++
	}
	return nil
}

func (p *AzureProvisioner) Destroy(ctx context.Context, prov Provisioned) error {
	rg := strings.TrimSpace(prov.InstanceID)
	if rg == "" {
		return nil
	}
	// Deleting the group reaps the VM, NIC, NSG, public IP and disk in one call.
	// --no-wait returns immediately; Azure completes it in the background.
	if _, err := runCLI(ctx, azureCLI, "group", "delete",
		"--name", rg, "--yes", "--no-wait"); err != nil {
		return fmt.Errorf("deleting Azure resource group %s: %w", rg, err)
	}
	return nil
}
