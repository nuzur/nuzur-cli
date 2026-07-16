package app

import (
	"testing"

	"github.com/nuzur/nuzur-cli/deploy"
)

func TestDeployFirewallRules(t *testing.T) {
	has := func(rules []deploy.FirewallRule, port, end int) bool {
		for _, r := range rules {
			if r.Port == port && r.PortEnd == end {
				return true
			}
		}
		return false
	}

	// db-only: SSH only.
	dbOnly := deployFirewallRules(true, "")
	if len(dbOnly) != 1 || !has(dbOnly, 22, 0) {
		t.Errorf("db-only should open only SSH; got %+v", dbOnly)
	}

	// full app, IP-only: SSH + 80 + 443 + auto-port range.
	ipOnly := deployFirewallRules(false, "")
	for _, tc := range []struct{ port, end int }{{22, 0}, {80, 0}, {443, 0}, {8443, 8542}} {
		if !has(ipOnly, tc.port, tc.end) {
			t.Errorf("IP-only full app missing rule %d-%d; got %+v", tc.port, tc.end, ipOnly)
		}
	}

	// full app, with domain: SSH + 80 + 443, no auto-port range.
	domain := deployFirewallRules(false, "app.example.com")
	if has(domain, 8443, 8542) {
		t.Errorf("domain deploy should not open the auto-port range; got %+v", domain)
	}
	if !has(domain, 443, 0) || !has(domain, 80, 0) || !has(domain, 22, 0) {
		t.Errorf("domain deploy missing 22/80/443; got %+v", domain)
	}
}
