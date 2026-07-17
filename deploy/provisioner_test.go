package deploy

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"
)

// --- test doubles for the provider CLI ---------------------------------------

type cliCall struct {
	name string
	args []string
}

// stubCLI records provider-CLI invocations and returns scripted output. It also
// stubs lookPath (so "installed" checks pass) and sshReady (so no real network
// wait happens). Everything is restored via t.Cleanup.
func stubCLI(t *testing.T, handler func(name string, args []string) (string, error)) *[]cliCall {
	t.Helper()
	// Pin the resource-name suffix so generated names are assertable. Real names
	// get a random tail (see providerResourceName) — that randomness is what stops
	// a "nuzur-<identifier>" collision with the user's own resources.
	origSuffix := providerNameSuffix
	providerNameSuffix = func() (string, error) { return "abc123", nil }
	t.Cleanup(func() { providerNameSuffix = origSuffix })
	var calls []cliCall
	origCLI, origLook, origReady := cliRunner, lookPath, sshReady
	cliRunner = func(ctx context.Context, name string, args ...string) (string, error) {
		calls = append(calls, cliCall{name: name, args: append([]string(nil), args...)})
		return handler(name, args)
	}
	lookPath = func(string) (string, error) { return "/usr/local/bin/stub", nil }
	sshReady = func(ctx context.Context, t Target, d time.Duration) error { return nil }
	t.Cleanup(func() { cliRunner, lookPath, sshReady = origCLI, origLook, origReady })
	return &calls
}

func joined(args []string) string { return strings.Join(args, " ") }

func findCall(calls []cliCall, mustContain ...string) *cliCall {
	for i := range calls {
		full := calls[i].name + " " + joined(calls[i].args)
		ok := true
		for _, s := range mustContain {
			if !strings.Contains(full, s) {
				ok = false
				break
			}
		}
		if ok {
			return &calls[i]
		}
	}
	return nil
}

// --- factory -----------------------------------------------------------------

func TestNewProvisioner(t *testing.T) {
	cases := []struct {
		provider Provider
		wantType string
		wantErr  string
	}{
		{ProviderSSH, "*deploy.SSHProvisioner", ""},
		{"", "*deploy.SSHProvisioner", ""},
		{ProviderDigitalOcean, "*deploy.DigitalOceanProvisioner", ""},
		{ProviderHetzner, "*deploy.HetznerProvisioner", ""},
		{ProviderLinode, "*deploy.LinodeProvisioner", ""},
		{ProviderGCP, "*deploy.GCPProvisioner", ""},
		{ProviderAzure, "*deploy.AzureProvisioner", ""},
		{ProviderVultr, "*deploy.VultrProvisioner", ""},
		{ProviderScaleway, "*deploy.ScalewayProvisioner", ""},
		{ProviderAWS, "", "planned but not available"},
		{Provider("bogus"), "", "unknown provider"},
	}
	for _, tc := range cases {
		t.Run(string(tc.provider), func(t *testing.T) {
			p, err := NewProvisioner(tc.provider)
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("err = %v, want containing %q", err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got := fmt.Sprintf("%T", p); got != tc.wantType {
				t.Errorf("type = %s, want %s", got, tc.wantType)
			}
		})
	}
}

// --- DigitalOcean ------------------------------------------------------------

func TestDigitalOceanProvision(t *testing.T) {
	calls := stubCLI(t, func(name string, args []string) (string, error) {
		switch {
		case findArg(args, "ssh-key") && findArg(args, "list"):
			return "999\tnuzur-deploy", nil // key already present
		case findArg(args, "droplet") && findArg(args, "create"):
			return "3164444  159.203.10.20", nil
		case findArg(args, "account"):
			return "ok", nil
		}
		return "", nil
	})

	p := NewDigitalOceanProvisioner()
	prov, err := p.Provision(context.Background(), Spec{
		Identifier:     "sfapi",
		Target:         Target{KeyPath: "/home/me/.ssh/id_ed25519"},
		ProviderConfig: ProviderConfig{Region: "nyc3"},
	})
	if err != nil {
		t.Fatalf("Provision error: %v", err)
	}
	if prov.InstanceID != "3164444" || prov.Target.Host != "159.203.10.20" || prov.Region != "nyc3" {
		t.Fatalf("unexpected Provisioned: %+v", prov)
	}
	if prov.Target.User != "root" || prov.Target.Port != 22 {
		t.Errorf("expected root@:22, got %s:%d", prov.Target.User, prov.Target.Port)
	}
	if findCall(*calls, "doctl", "account", "get") == nil {
		t.Errorf("expected the doctl auth check; calls: %v", *calls)
	}
	create := findCall(*calls, "droplet", "create", "--region", "nyc3", "--ssh-keys", "999", "--wait")
	if create == nil {
		t.Errorf("droplet create argv missing/incorrect; calls: %v", *calls)
	}
	if findCall(*calls, "--image", doDefaultImage) == nil {
		t.Errorf("expected default image %s in create argv", doDefaultImage)
	}
}

// Region is optional: omitting it uses the provider default rather than failing.
func TestDigitalOceanRegionDefaults(t *testing.T) {
	calls := stubCLI(t, func(name string, args []string) (string, error) {
		switch {
		case findArg(args, "ssh-key") && findArg(args, "list"):
			return "999\tnuzur-deploy", nil // key already present
		case findArg(args, "droplet") && findArg(args, "create"):
			return "3164444  159.203.10.20", nil
		}
		return "", nil
	})
	prov, err := NewDigitalOceanProvisioner().Provision(context.Background(), Spec{
		Identifier: "x",
		Target:     Target{KeyPath: "/home/me/.ssh/id_ed25519"},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if prov.Region != doDefaultRegion {
		t.Errorf("Region = %q, want the default %q", prov.Region, doDefaultRegion)
	}
	if findCall(*calls, "droplet", "create", "--region", doDefaultRegion) == nil {
		t.Errorf("create should use the default region; calls: %v", *calls)
	}
}

func TestDigitalOceanConfigureFirewall(t *testing.T) {
	calls := stubCLI(t, func(string, []string) (string, error) { return "", nil })
	err := NewDigitalOceanProvisioner().ConfigureFirewall(context.Background(),
		Provisioned{InstanceID: "42"},
		[]FirewallRule{{Port: 22}, {Port: 80}, {Port: 443}, {Port: 8443, PortEnd: 8542}})
	if err != nil {
		t.Fatalf("ConfigureFirewall error: %v", err)
	}
	fw := findCall(*calls, "firewall", "create", "--droplet-ids", "42")
	if fw == nil {
		t.Fatalf("firewall create argv missing; calls: %v", *calls)
	}
	inbound := joined(fw.args)
	for _, want := range []string{"ports:22", "ports:80", "ports:443", "ports:8443-8542"} {
		if !strings.Contains(inbound, want) {
			t.Errorf("firewall inbound rules missing %q; got %s", want, inbound)
		}
	}
}

func TestDigitalOceanDestroy(t *testing.T) {
	calls := stubCLI(t, func(name string, args []string) (string, error) {
		if findArg(args, "firewall") && findArg(args, "list") {
			return "77\tnuzur-fw-42", nil
		}
		return "", nil
	})
	if err := NewDigitalOceanProvisioner().Destroy(context.Background(), Provisioned{InstanceID: "42"}); err != nil {
		t.Fatalf("Destroy error: %v", err)
	}
	if findCall(*calls, "droplet", "delete", "42", "--force") == nil {
		t.Errorf("expected droplet delete argv; calls: %v", *calls)
	}
	if findCall(*calls, "firewall", "delete", "77") == nil {
		t.Errorf("expected firewall delete for resolved id 77; calls: %v", *calls)
	}
}

// --- Hetzner -----------------------------------------------------------------

func TestHetznerProvision(t *testing.T) {
	calls := stubCLI(t, func(name string, args []string) (string, error) {
		switch {
		case findArg(args, "ssh-key") && findArg(args, "describe"):
			return "nuzur-deploy", nil // key exists
		case findArg(args, "server") && findArg(args, "describe"):
			return "8675309", nil
		case findArg(args, "server") && findArg(args, "ip"):
			return "49.12.0.5", nil
		}
		return "", nil
	})
	p := NewHetznerProvisioner()
	prov, err := p.Provision(context.Background(), Spec{
		Identifier:     "sfapi",
		ProviderConfig: ProviderConfig{Region: "nbg1", SSHKeyName: "mykey"},
	})
	if err != nil {
		t.Fatalf("Provision error: %v", err)
	}
	if prov.InstanceID != "8675309" || prov.Target.Host != "49.12.0.5" {
		t.Fatalf("unexpected Provisioned: %+v", prov)
	}
	if findCall(*calls, "hcloud", "context", "active") == nil {
		t.Errorf("expected the hcloud auth check; calls: %v", *calls)
	}
	create := findCall(*calls, "server", "create", "--location", "nbg1", "--ssh-key", "mykey")
	if create == nil {
		t.Errorf("server create argv missing/incorrect; calls: %v", *calls)
	}
	if findCall(*calls, "--type", hetznerDefaultType) == nil {
		t.Errorf("expected default type %s", hetznerDefaultType)
	}
}

func TestHetznerConfigureFirewall(t *testing.T) {
	calls := stubCLI(t, func(string, []string) (string, error) { return "", nil })
	err := NewHetznerProvisioner().ConfigureFirewall(context.Background(),
		Provisioned{InstanceID: "42"},
		[]FirewallRule{{Port: 22}, {Port: 8443, PortEnd: 8542}})
	if err != nil {
		t.Fatalf("ConfigureFirewall error: %v", err)
	}
	if findCall(*calls, "firewall", "add-rule", "--port", "22") == nil {
		t.Errorf("expected add-rule for port 22; calls: %v", *calls)
	}
	if findCall(*calls, "firewall", "add-rule", "--port", "8443-8542") == nil {
		t.Errorf("expected add-rule for range 8443-8542; calls: %v", *calls)
	}
	if findCall(*calls, "firewall", "apply-to-resource", "--server", "42") == nil {
		t.Errorf("expected apply-to-resource; calls: %v", *calls)
	}
}

func TestHetznerMissingNamedKey(t *testing.T) {
	stubCLI(t, func(name string, args []string) (string, error) {
		if findArg(args, "ssh-key") && findArg(args, "describe") {
			return "", fmt.Errorf("ssh key not found")
		}
		return "", nil
	})
	_, err := NewHetznerProvisioner().Provision(context.Background(), Spec{
		Identifier:     "x",
		ProviderConfig: ProviderConfig{Region: "nbg1", SSHKeyName: "ghost"},
	})
	if err == nil || !strings.Contains(err.Error(), "no Hetzner SSH key named") {
		t.Fatalf("expected missing-key error, got %v", err)
	}
}

func findArg(args []string, want string) bool {
	for _, a := range args {
		if a == want {
			return true
		}
	}
	return false
}
