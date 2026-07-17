package deploy

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// testPubKeyPath writes a throwaway keypair and returns the PRIVATE key path.
// Linode passes the public key inline at create, so unlike DigitalOcean there is
// no registered-key lookup to short-circuit — resolveSSHPublicKey always reads
// the .pub sibling off disk.
func testPubKeyPath(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	priv := filepath.Join(dir, "id_test")
	if err := os.WriteFile(priv+".pub", []byte("ssh-ed25519 AAAATESTKEY nuzur-test\n"), 0o600); err != nil {
		t.Fatalf("writing test pubkey: %v", err)
	}
	return priv
}

// linodeHandler scripts the linode-cli calls a happy-path provision makes.
func linodeHandler(t *testing.T) func(name string, args []string) (string, error) {
	t.Helper()
	return func(name string, args []string) (string, error) {
		switch {
		case findArg(args, "linodes") && findArg(args, "create"):
			return "4711  139.162.1.20", nil
		case findArg(args, "firewalls") && findArg(args, "create"):
			return "900", nil
		}
		return "", nil
	}
}

func TestLinodeProvision(t *testing.T) {
	calls := stubCLI(t, linodeHandler(t))

	p := NewLinodeProvisioner()
	prov, err := p.Provision(context.Background(), Spec{
		Identifier:     "sfapi",
		ProviderConfig: ProviderConfig{Region: "us-east"},
		Target:         Target{KeyPath: testPubKeyPath(t)},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if prov.InstanceID != "4711" {
		t.Errorf("InstanceID = %q, want 4711", prov.InstanceID)
	}
	if prov.Target.Host != "139.162.1.20" {
		t.Errorf("Host = %q, want 139.162.1.20", prov.Target.Host)
	}
	if prov.Target.User != "root" || prov.Target.Port != 22 {
		t.Errorf("Target = %+v, want root@:22", prov.Target)
	}
	if prov.Region != "us-east" {
		t.Errorf("Region = %q, want us-east", prov.Region)
	}

	// The auth probe must hit an endpoint that actually REQUIRES a token:
	// `regions list` is public and would pass on an unauthenticated CLI.
	if findCall(*calls, "linode-cli", "account", "view") == nil {
		t.Errorf("expected an auth-requiring linode-cli check; calls: %+v", *calls)
	}
	if findCall(*calls, "linode-cli", "regions", "list") != nil {
		t.Errorf("regions list is a PUBLIC endpoint — useless as an auth probe; calls: %+v", *calls)
	}
	// Create carries the defaults and the INLINE public key (Linode registers no key).
	c := findCall(*calls, "linodes", "create")
	if c == nil {
		t.Fatalf("no linodes create call; calls: %+v", *calls)
	}
	full := joined(c.args)
	for _, want := range []string{
		"--label nuzur-sfapi-abc123", "--region us-east",
		"--type " + linodeDefaultType, "--image " + linodeDefaultImage,
		"--authorized_keys", "--root_pass",
		"--format id,ipv4",
	} {
		if !strings.Contains(full, want) {
			t.Errorf("create args missing %q; got: %s", want, full)
		}
	}
}

// Region is optional: omitting it uses the provider default rather than failing.
func TestLinodeRegionDefaults(t *testing.T) {
	calls := stubCLI(t, linodeHandler(t))
	prov, err := NewLinodeProvisioner().Provision(context.Background(), Spec{
		Identifier: "sfapi",
		Target:     Target{KeyPath: testPubKeyPath(t)},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if prov.Region != linodeDefaultRegion {
		t.Errorf("Region = %q, want the default %q", prov.Region, linodeDefaultRegion)
	}
	if findCall(*calls, "linodes", "create", "--region", linodeDefaultRegion) == nil {
		t.Errorf("create should use the default region; calls: %+v", *calls)
	}
}

func TestLinodeConfigureFirewall(t *testing.T) {
	calls := stubCLI(t, linodeHandler(t))

	err := NewLinodeProvisioner().ConfigureFirewall(context.Background(),
		Provisioned{InstanceID: "4711"},
		[]FirewallRule{{Port: 22}, {Port: 80}, {Port: 443}, {Port: 8443, PortEnd: 8542}})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	c := findCall(*calls, "firewalls", "create")
	if c == nil {
		t.Fatalf("no firewalls create call; calls: %+v", *calls)
	}
	full := joined(c.args)
	if !strings.Contains(full, "--label nuzur-fw-4711") {
		t.Errorf("firewall label missing; got: %s", full)
	}
	if !strings.Contains(full, "--rules.inbound_policy DROP") {
		t.Errorf("inbound policy should default-deny; got: %s", full)
	}

	// The inbound rules are a JSON array — parse them rather than substring-match,
	// so a malformed payload fails loudly here instead of at the API.
	raw := findFlagValue(c.args, "--rules.inbound")
	if raw == "" {
		t.Fatalf("no --rules.inbound payload; got: %s", full)
	}
	var rules []linodeFWRule
	if err := json.Unmarshal([]byte(raw), &rules); err != nil {
		t.Fatalf("inbound rules are not valid JSON (%v): %s", err, raw)
	}
	gotPorts := map[string]bool{}
	for _, r := range rules {
		gotPorts[r.Ports] = true
		if r.Protocol != "TCP" || r.Action != "ACCEPT" {
			t.Errorf("rule %+v: want TCP/ACCEPT", r)
		}
	}
	for _, want := range []string{"22", "80", "443", "8443-8542"} {
		if !gotPorts[want] {
			t.Errorf("inbound rules missing port %q; got %v", want, gotPorts)
		}
	}

	// ...and it gets attached to the linode.
	if findCall(*calls, "firewalls", "device-create", "900") == nil {
		t.Errorf("firewall not attached to the linode; calls: %+v", *calls)
	}
}

func TestLinodeDestroy(t *testing.T) {
	calls := stubCLI(t, func(name string, args []string) (string, error) {
		if findArg(args, "firewalls") && findArg(args, "list") {
			return "900  nuzur-fw-4711", nil
		}
		return "", nil
	})

	if err := NewLinodeProvisioner().Destroy(context.Background(), Provisioned{InstanceID: "4711"}); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if findCall(*calls, "firewalls", "delete", "900") == nil {
		t.Errorf("firewall not deleted; calls: %+v", *calls)
	}
	if findCall(*calls, "linodes", "delete", "4711") == nil {
		t.Errorf("linode not deleted; calls: %+v", *calls)
	}
}

// An empty InstanceID must be a no-op, not a delete-everything.
func TestLinodeDestroyNoInstance(t *testing.T) {
	calls := stubCLI(t, linodeHandler(t))
	if err := NewLinodeProvisioner().Destroy(context.Background(), Provisioned{}); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(*calls) != 0 {
		t.Errorf("expected no CLI calls, got: %+v", *calls)
	}
}

// The generated root password is throwaway but must satisfy Linode's strength
// rule (it rejects weak ones at create).
func TestLinodeRootPassword(t *testing.T) {
	seen := map[string]bool{}
	for i := 0; i < 3; i++ {
		pw, err := linodeRootPassword()
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if len(pw) < 11 {
			t.Errorf("password too short for Linode (%d): %q", len(pw), pw)
		}
		if seen[pw] {
			t.Errorf("password repeated across calls: %q", pw)
		}
		seen[pw] = true
	}
}

// findFlagValue returns the value following an exact flag token.
func findFlagValue(args []string, flag string) string {
	for i, a := range args {
		if a == flag && i+1 < len(args) {
			return args[i+1]
		}
	}
	return ""
}
