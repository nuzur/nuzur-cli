package deploy

import (
	"context"
	"strings"
	"testing"
)

// gcpHandler scripts the gcloud calls a happy-path provision makes. Note the
// create returns NAME + IP (not a numeric id) — see the adapter's file comment.
func gcpHandler() func(name string, args []string) (string, error) {
	return func(name string, args []string) (string, error) {
		switch {
		case findArg(args, "config") && findArg(args, "get-value"):
			return "my-project", nil
		case findArg(args, "instances") && findArg(args, "create"):
			return "nuzur-sfapi  34.72.1.5", nil
		}
		return "", nil
	}
}

func TestGCPProvision(t *testing.T) {
	calls := stubCLI(t, gcpHandler())

	prov, err := NewGCPProvisioner().Provision(context.Background(), Spec{
		Identifier:     "sfapi",
		ProviderConfig: ProviderConfig{Region: "us-central1-a"},
		Target:         Target{KeyPath: testPubKeyPath(t)},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// InstanceID is the instance NAME, which is what makes the tag-targeted
	// firewall (and the zone-scoped delete) resolvable from Provisioned alone.
	if prov.InstanceID != "nuzur-sfapi" {
		t.Errorf("InstanceID = %q, want the instance name nuzur-sfapi", prov.InstanceID)
	}
	if prov.Target.Host != "34.72.1.5" || prov.Target.User != "root" || prov.Target.Port != 22 {
		t.Errorf("Target = %+v, want root@34.72.1.5:22", prov.Target)
	}
	if prov.Region != "us-central1-a" {
		t.Errorf("Region = %q, want the zone us-central1-a", prov.Region)
	}

	if findCall(*calls, "gcloud", "config", "get-value", "project") == nil {
		t.Errorf("expected the gcloud project/auth check; calls: %+v", *calls)
	}
	c := findCall(*calls, "instances", "create")
	if c == nil {
		t.Fatalf("no instances create call; calls: %+v", *calls)
	}
	full := joined(c.args)
	for _, want := range []string{
		"--zone us-central1-a",
		"--machine-type " + gcpDefaultMachineType,
		"--image-family " + gcpDefaultImageFamily,
		"--image-project " + gcpImageProject,
		"--tags nuzur-sfapi",        // the tag the firewall targets
		"--metadata ssh-keys=root:", // key injected via metadata, not registered
	} {
		if !strings.Contains(full, want) {
			t.Errorf("create args missing %q; got: %s", want, full)
		}
	}
}

func TestGCPRegionRequiredIsAZone(t *testing.T) {
	stubCLI(t, gcpHandler())
	_, err := NewGCPProvisioner().Provision(context.Background(), Spec{Identifier: "sfapi"})
	if err == nil || !strings.Contains(err.Error(), "--region is required") {
		t.Fatalf("err = %v, want a --region required error", err)
	}
	// The message must say ZONE — passing a bare region (us-central1) is the
	// obvious mistake and gcloud's own error for it is opaque.
	if !strings.Contains(err.Error(), "ZONE") {
		t.Errorf("error should tell the user --region takes a zone; got: %v", err)
	}
}

// An unset gcloud project must fail up front with an actionable message rather
// than deep inside the create.
func TestGCPUnsetProject(t *testing.T) {
	stubCLI(t, func(name string, args []string) (string, error) {
		if findArg(args, "config") && findArg(args, "get-value") {
			return "(unset)", nil
		}
		return "", nil
	})
	_, err := NewGCPProvisioner().Provision(context.Background(), Spec{
		Identifier:     "sfapi",
		ProviderConfig: ProviderConfig{Region: "us-central1-a"},
	})
	if err == nil || !strings.Contains(err.Error(), "gcloud config set project") {
		t.Fatalf("err = %v, want an actionable unset-project error", err)
	}
}

func TestGCPConfigureFirewall(t *testing.T) {
	calls := stubCLI(t, gcpHandler())

	err := NewGCPProvisioner().ConfigureFirewall(context.Background(),
		Provisioned{InstanceID: "nuzur-sfapi", Region: "us-central1-a"},
		[]FirewallRule{{Port: 22}, {Port: 80}, {Port: 443}, {Port: 8443, PortEnd: 8542}})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	c := findCall(*calls, "firewall-rules", "create")
	if c == nil {
		t.Fatalf("no firewall-rules create call; calls: %+v", *calls)
	}
	full := joined(c.args)
	// The rule must be scoped to THIS instance via its tag — a network-level rule
	// without target-tags would open every instance in the project.
	if !strings.Contains(full, "--target-tags nuzur-sfapi") {
		t.Errorf("firewall rule not scoped to the instance tag; got: %s", full)
	}
	if !strings.Contains(full, "nuzur-fw-nuzur-sfapi") {
		t.Errorf("firewall rule name missing; got: %s", full)
	}
	if !strings.Contains(full, "--allow tcp:22,tcp:80,tcp:443,tcp:8443-8542") {
		t.Errorf("allow list wrong; got: %s", full)
	}
}

func TestGCPDestroy(t *testing.T) {
	calls := stubCLI(t, gcpHandler())

	err := NewGCPProvisioner().Destroy(context.Background(),
		Provisioned{InstanceID: "nuzur-sfapi", Region: "us-central1-a"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if findCall(*calls, "firewall-rules", "delete", "nuzur-fw-nuzur-sfapi") == nil {
		t.Errorf("firewall rule not deleted; calls: %+v", *calls)
	}
	// Deleting an instance is zone-scoped; Region carries the zone.
	if findCall(*calls, "instances", "delete", "nuzur-sfapi", "--zone", "us-central1-a") == nil {
		t.Errorf("instance not deleted with its zone; calls: %+v", *calls)
	}
}

func TestGCPDestroyNoInstance(t *testing.T) {
	calls := stubCLI(t, gcpHandler())
	if err := NewGCPProvisioner().Destroy(context.Background(), Provisioned{}); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(*calls) != 0 {
		t.Errorf("expected no CLI calls, got: %+v", *calls)
	}
}
