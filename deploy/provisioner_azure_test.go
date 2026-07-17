package deploy

import (
	"context"
	"strings"
	"testing"
)

func azureHandler() func(name string, args []string) (string, error) {
	return func(name string, args []string) (string, error) {
		switch {
		case findArg(args, "vm") && findArg(args, "create"):
			return "20.51.1.9", nil // --query publicIpAddress -o tsv
		case findArg(args, "account") && findArg(args, "show"):
			return "{}", nil
		}
		return "", nil
	}
}

func TestAzureProvision(t *testing.T) {
	calls := stubCLI(t, azureHandler())

	prov, err := NewAzureProvisioner().Provision(context.Background(), Spec{
		Identifier:     "sfapi",
		ProviderConfig: ProviderConfig{Region: "eastus"},
		Target:         Target{KeyPath: testPubKeyPath(t)},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// THE Azure divergence: Azure forbids root as admin username, so this is the
	// one adapter whose SSH user isn't root. The deploy compensates by enabling
	// sudo for any non-root user — if this regresses to "root", the VM create
	// fails at the provider.
	if prov.Target.User != azureAdminUser {
		t.Errorf("Target.User = %q, want %q (Azure rejects root)", prov.Target.User, azureAdminUser)
	}
	if prov.Target.User == "root" {
		t.Error("Azure must never use root as the admin username")
	}
	if prov.Target.Host != "20.51.1.9" || prov.Target.Port != 22 {
		t.Errorf("Target = %+v, want 20.51.1.9:22", prov.Target)
	}
	// InstanceID is the RESOURCE GROUP — that's what Destroy deletes.
	if prov.InstanceID != "nuzur-sfapi-abc123" {
		t.Errorf("InstanceID = %q, want the resource group nuzur-sfapi-abc123", prov.InstanceID)
	}

	if findCall(*calls, "az", "account", "show") == nil {
		t.Errorf("expected the az auth check; calls: %+v", *calls)
	}
	// The resource group must be created BEFORE the VM.
	g, v := findCall(*calls, "group", "create"), findCall(*calls, "vm", "create")
	if g == nil {
		t.Fatalf("no group create call; calls: %+v", *calls)
	}
	if v == nil {
		t.Fatalf("no vm create call; calls: %+v", *calls)
	}
	full := joined(v.args)
	for _, want := range []string{
		"--resource-group nuzur-sfapi-abc123", "--name nuzur-sfapi-abc123",
		"--image " + azureDefaultImage, "--size " + azureDefaultSize,
		"--location eastus", "--admin-username " + azureAdminUser,
		"--ssh-key-values", "--query publicIpAddress",
	} {
		if !strings.Contains(full, want) {
			t.Errorf("vm create args missing %q; got: %s", want, full)
		}
	}
}

// Region is optional: omitting it uses the provider default rather than failing.
func TestAzureRegionDefaults(t *testing.T) {
	calls := stubCLI(t, azureHandler())
	prov, err := NewAzureProvisioner().Provision(context.Background(), Spec{
		Identifier: "sfapi",
		Target:     Target{KeyPath: testPubKeyPath(t)},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if prov.Region != azureDefaultRegion {
		t.Errorf("Region = %q, want the default %q", prov.Region, azureDefaultRegion)
	}
	if findCall(*calls, "group", "create", "--location", azureDefaultRegion) == nil {
		t.Errorf("group create should use the default region; calls: %+v", *calls)
	}
}

// Azure rejects duplicate NSG priorities, so each opened port must get its own.
func TestAzureConfigureFirewallPriorities(t *testing.T) {
	calls := stubCLI(t, azureHandler())

	err := NewAzureProvisioner().ConfigureFirewall(context.Background(),
		Provisioned{InstanceID: "nuzur-sfapi", Region: "eastus"},
		[]FirewallRule{{Port: 22}, {Port: 80}, {Port: 443}, {Port: 8443, PortEnd: 8542}})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	wantPorts := []string{"22", "80", "443", "8443-8542"}
	seenPriority := map[string]bool{}
	for _, want := range wantPorts {
		c := findCall(*calls, "open-port", "--port", want)
		if c == nil {
			t.Errorf("port %s never opened; calls: %+v", want, *calls)
			continue
		}
		pr := findFlagValue(c.args, "--priority")
		if pr == "" {
			t.Errorf("port %s opened without an explicit priority", want)
			continue
		}
		if seenPriority[pr] {
			t.Errorf("priority %s reused — Azure rejects duplicates", pr)
		}
		seenPriority[pr] = true
	}
	if len(seenPriority) != len(wantPorts) {
		t.Errorf("expected %d distinct priorities, got %d", len(wantPorts), len(seenPriority))
	}
}

// Destroy is a single group delete — that's the whole point of the
// resource-group-per-deployment design (the VM, NIC, NSG, IP and disk go too).
func TestAzureDestroyDeletesResourceGroup(t *testing.T) {
	calls := stubCLI(t, azureHandler())

	err := NewAzureProvisioner().Destroy(context.Background(),
		Provisioned{InstanceID: "nuzur-sfapi", Region: "eastus"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	c := findCall(*calls, "group", "delete", "--name", "nuzur-sfapi", "--yes")
	if c == nil {
		t.Fatalf("resource group not deleted; calls: %+v", *calls)
	}
	// Deleting only the VM would orphan the NIC/NSG/IP/disk (and keep billing).
	if findCall(*calls, "vm", "delete") != nil {
		t.Error("should delete the resource group, not the VM individually")
	}
}

func TestAzureDestroyNoInstance(t *testing.T) {
	calls := stubCLI(t, azureHandler())
	if err := NewAzureProvisioner().Destroy(context.Background(), Provisioned{}); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(*calls) != 0 {
		t.Errorf("expected no CLI calls, got: %+v", *calls)
	}
}
