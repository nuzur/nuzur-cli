package deploy

import (
	"context"
	"strings"
	"testing"
	"time"
)

// --- Vultr --------------------------------------------------------------------

func vultrHandler(createIP string) func(name string, args []string) (string, error) {
	return func(name string, args []string) (string, error) {
		switch {
		case findArg(args, "ssh-key") && findArg(args, "list"):
			return `{"ssh_keys":[{"id":"key-1","name":"nuzur-deploy"}]}`, nil
		case findArg(args, "instance") && findArg(args, "create"):
			return `{"instance":{"id":"inst-1","main_ip":"` + createIP + `"}}`, nil
		case findArg(args, "instance") && findArg(args, "get"):
			return `{"instance":{"id":"inst-1","main_ip":"66.42.1.7"}}`, nil
		case findArg(args, "group") && findArg(args, "create"):
			return `{"firewall_group":{"id":"fw-1"}}`, nil
		}
		return "", nil
	}
}

func TestVultrProvision(t *testing.T) {
	calls := stubCLI(t, vultrHandler("66.42.1.7"))

	prov, err := NewVultrProvisioner().Provision(context.Background(), Spec{
		Identifier:     "sfapi",
		ProviderConfig: ProviderConfig{Region: "ewr"},
		Target:         Target{KeyPath: testPubKeyPath(t)},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if prov.InstanceID != "inst-1" || prov.Target.Host != "66.42.1.7" {
		t.Errorf("got %+v, want inst-1 @ 66.42.1.7", prov)
	}
	// The real vultr-cli has no `account get`; the probe must be `account info`.
	if findCall(*calls, "vultr-cli", "account", "info") == nil {
		t.Errorf("expected the `account info` auth check; calls: %+v", *calls)
	}
	c := findCall(*calls, "instance", "create")
	if c == nil {
		t.Fatalf("no instance create; calls: %+v", *calls)
	}
	full := joined(c.args)
	for _, want := range []string{"--region ewr", "--plan " + vultrDefaultPlan, "--os " + vultrDefaultOS, "--ssh-keys key-1", "-o json"} {
		if !strings.Contains(full, want) {
			t.Errorf("create args missing %q; got: %s", want, full)
		}
	}
}

// Vultr hands back main_ip=0.0.0.0 for a few seconds; the adapter must poll
// rather than hand that to the SSH wait.
func TestVultrWaitsForAsyncIP(t *testing.T) {
	orig := vultrIPPoll
	vultrIPPoll = time.Millisecond
	t.Cleanup(func() { vultrIPPoll = orig })

	calls := stubCLI(t, vultrHandler("0.0.0.0"))

	prov, err := NewVultrProvisioner().Provision(context.Background(), Spec{
		Identifier:     "sfapi",
		ProviderConfig: ProviderConfig{Region: "ewr"},
		Target:         Target{KeyPath: testPubKeyPath(t)},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if prov.Target.Host != "66.42.1.7" {
		t.Errorf("Host = %q, want the polled IP 66.42.1.7 (not 0.0.0.0)", prov.Target.Host)
	}
	if findCall(*calls, "instance", "get", "inst-1") == nil {
		t.Errorf("expected an instance get poll for the IP; calls: %+v", *calls)
	}
}

func TestVultrConfigureFirewall(t *testing.T) {
	calls := stubCLI(t, vultrHandler("66.42.1.7"))

	err := NewVultrProvisioner().ConfigureFirewall(context.Background(),
		Provisioned{InstanceID: "inst-1", Region: "ewr"},
		[]FirewallRule{{Port: 22}, {Port: 8443, PortEnd: 8542}})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// The group id is POSITIONAL and --ip-type is required — vultr-cli rejects the
	// `--id=` form its own help example shows.
	c := findCall(*calls, "firewall", "rule", "create", "fw-1", "--ip-type", "v4")
	if c == nil {
		t.Fatalf("rule create should pass the group id positionally with --ip-type; calls: %+v", *calls)
	}
	if findCall(*calls, "rule", "create", "--port", "8443:8542") == nil {
		t.Errorf("port range should use Vultr's colon form; calls: %+v", *calls)
	}
	// Attaching is its own subcommand — `instance update` has no firewall flag.
	if findCall(*calls, "instance", "update-firewall-group", "inst-1", "--firewall-group-id", "fw-1") == nil {
		t.Errorf("firewall group not attached via update-firewall-group; calls: %+v", *calls)
	}
}

func TestVultrDestroy(t *testing.T) {
	calls := stubCLI(t, func(name string, args []string) (string, error) {
		if findArg(args, "group") && findArg(args, "list") {
			return `{"firewall_groups":[{"id":"fw-1","description":"nuzur-fw-inst-1"}]}`, nil
		}
		return "", nil
	})
	if err := NewVultrProvisioner().Destroy(context.Background(), Provisioned{InstanceID: "inst-1"}); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if findCall(*calls, "instance", "delete", "inst-1") == nil {
		t.Errorf("instance not deleted; calls: %+v", *calls)
	}
	if findCall(*calls, "firewall", "group", "delete", "fw-1") == nil {
		t.Errorf("firewall group not deleted; calls: %+v", *calls)
	}
}

// --- Scaleway -----------------------------------------------------------------

func scalewayHandler(keys string) func(name string, args []string) (string, error) {
	return func(name string, args []string) (string, error) {
		switch {
		case findArg(args, "iam") && findArg(args, "ssh-key"):
			return keys, nil
		case findArg(args, "server") && findArg(args, "create"):
			return "srv-1  51.15.1.3", nil
		case findArg(args, "security-group") && findArg(args, "create"):
			return "sg-1", nil
		case findArg(args, "config") && findArg(args, "get"):
			return "proj-1", nil
		}
		return "", nil
	}
}

func TestScalewayProvision(t *testing.T) {
	calls := stubCLI(t, scalewayHandler("key-1"))

	prov, err := NewScalewayProvisioner().Provision(context.Background(), Spec{
		Identifier:     "sfapi",
		ProviderConfig: ProviderConfig{Region: "fr-par-1"},
		Target:         Target{KeyPath: testPubKeyPath(t)},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if prov.InstanceID != "srv-1" || prov.Target.Host != "51.15.1.3" {
		t.Errorf("got %+v, want srv-1 @ 51.15.1.3", prov)
	}
	// SSH keys live under `iam` in current scw; `account ssh-key list` prints help
	// instead of erroring, so a regression there would silently break the check.
	if findCall(*calls, "scw", "iam", "ssh-key", "list") == nil {
		t.Errorf("expected the account SSH-key probe under `iam`; calls: %+v", *calls)
	}
	if findCall(*calls, "scw", "config", "get", "default-project-id") == nil {
		t.Errorf("expected the scw auth check; calls: %+v", *calls)
	}
	c := findCall(*calls, "server", "create")
	if c == nil {
		t.Fatalf("no server create; calls: %+v", *calls)
	}
	full := joined(c.args)
	// scw takes key=value args, not --flags.
	for _, want := range []string{"name=nuzur-sfapi-abc123", "type=" + scalewayDefaultType, "image=" + scalewayDefaultImage, "zone=fr-par-1", "ip=new"} {
		if !strings.Contains(full, want) {
			t.Errorf("create args missing %q; got: %s", want, full)
		}
	}
}

// Scaleway only injects ACCOUNT ssh keys — with none registered the server would
// boot unreachable, so that must fail up front with an actionable message.
func TestScalewayNoAccountSSHKey(t *testing.T) {
	stubCLI(t, scalewayHandler("")) // no account keys registered
	_, err := NewScalewayProvisioner().Provision(context.Background(), Spec{
		Identifier:     "sfapi",
		ProviderConfig: ProviderConfig{Region: "fr-par-1"},
		Target:         Target{KeyPath: testPubKeyPath(t)},
	})
	if err == nil || !strings.Contains(err.Error(), "scw iam ssh-key create") {
		t.Fatalf("err = %v, want an actionable no-ssh-key error", err)
	}
}

// Region is optional; for Scaleway it carries a ZONE, so the default must be one.
func TestScalewayZoneDefaults(t *testing.T) {
	calls := stubCLI(t, scalewayHandler("key-1"))
	prov, err := NewScalewayProvisioner().Provision(context.Background(), Spec{
		Identifier: "sfapi",
		Target:     Target{KeyPath: testPubKeyPath(t)},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if prov.Region != scalewayDefaultZone {
		t.Errorf("Region = %q, want the default zone %q", prov.Region, scalewayDefaultZone)
	}
	if findCall(*calls, "server", "create", "zone="+scalewayDefaultZone) == nil {
		t.Errorf("create should use the default zone; calls: %+v", *calls)
	}
}

func TestScalewayConfigureFirewall(t *testing.T) {
	calls := stubCLI(t, scalewayHandler("key-1"))

	err := NewScalewayProvisioner().ConfigureFirewall(context.Background(),
		Provisioned{InstanceID: "srv-1", Region: "fr-par-1"},
		[]FirewallRule{{Port: 22}, {Port: 8443, PortEnd: 8542}})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if findCall(*calls, "security-group", "create", "inbound-default-policy=drop") == nil {
		t.Errorf("security group should default-deny inbound; calls: %+v", *calls)
	}
	if findCall(*calls, "create-rule", "dest-port-from=22", "dest-port-to=22") == nil {
		t.Errorf("single port should render as an equal from/to range; calls: %+v", *calls)
	}
	if findCall(*calls, "create-rule", "dest-port-from=8443", "dest-port-to=8542") == nil {
		t.Errorf("port range missing; calls: %+v", *calls)
	}
	if findCall(*calls, "server", "update", "srv-1", "security-group-id=sg-1") == nil {
		t.Errorf("security group not attached; calls: %+v", *calls)
	}
}

// Terminating must free the IP and volumes, or they keep billing after the
// server is gone.
func TestScalewayDestroyFreesIPAndVolumes(t *testing.T) {
	calls := stubCLI(t, scalewayHandler("key-1"))
	if err := NewScalewayProvisioner().Destroy(context.Background(),
		Provisioned{InstanceID: "srv-1", Region: "fr-par-1"}); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	c := findCall(*calls, "server", "terminate", "srv-1")
	if c == nil {
		t.Fatalf("server not terminated; calls: %+v", *calls)
	}
	full := joined(c.args)
	for _, want := range []string{"with-ip=true", "with-block=true"} {
		if !strings.Contains(full, want) {
			t.Errorf("terminate must %s or it keeps billing; got: %s", want, full)
		}
	}
}
