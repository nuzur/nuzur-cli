package app

import (
	"testing"

	"github.com/nuzur/nuzur-cli/deploy"
)

// A re-deploy must never erase the handles `destroy` uses to delete the VM —
// doing so leaks a running, billing droplet with no warning.
func TestCarryForwardProvisioning(t *testing.T) {
	managedPrior := func() *deploy.Deployment {
		return &deploy.Deployment{
			Provider:             deploy.ProviderDigitalOcean,
			ProviderInstanceID:   "512345678",
			ProviderResourceName: "nuzur-sfapi-abc123",
			Region:               "nyc3",
		}
	}

	t.Run("ssh re-deploy keeps the managed provisioning metadata", func(t *testing.T) {
		dep := &deploy.Deployment{Provider: deploy.ProviderSSH}
		carryForwardProvisioning(dep, managedPrior(), deploy.ProviderSSH)

		if dep.Provider != deploy.ProviderDigitalOcean {
			t.Errorf("Provider = %q, want %q — destroy skips the VM delete for ssh", dep.Provider, deploy.ProviderDigitalOcean)
		}
		if dep.ProviderInstanceID != "512345678" {
			t.Errorf("ProviderInstanceID = %q, want 512345678", dep.ProviderInstanceID)
		}
		if dep.ProviderResourceName != "nuzur-sfapi-abc123" {
			t.Errorf("ProviderResourceName = %q, want nuzur-sfapi-abc123", dep.ProviderResourceName)
		}
		if dep.Region != "nyc3" {
			t.Errorf("Region = %q, want nyc3", dep.Region)
		}
	})

	t.Run("ssh re-deploy keeps a name-only record", func(t *testing.T) {
		prior := managedPrior()
		prior.ProviderInstanceID = "" // interrupted during create: name is the only handle
		dep := &deploy.Deployment{Provider: deploy.ProviderSSH}
		carryForwardProvisioning(dep, prior, deploy.ProviderSSH)

		if dep.Provider != deploy.ProviderDigitalOcean || dep.ProviderResourceName != "nuzur-sfapi-abc123" {
			t.Errorf("got provider %q name %q, want digitalocean/nuzur-sfapi-abc123", dep.Provider, dep.ProviderResourceName)
		}
	})

	t.Run("managed re-deploy keeps its own fresh values", func(t *testing.T) {
		dep := &deploy.Deployment{
			Provider:             deploy.ProviderDigitalOcean,
			ProviderInstanceID:   "599999999",
			ProviderResourceName: "nuzur-sfapi-zzz999",
			Region:               "sfo3",
		}
		carryForwardProvisioning(dep, managedPrior(), deploy.ProviderDigitalOcean)

		if dep.ProviderInstanceID != "599999999" || dep.Region != "sfo3" {
			t.Errorf("stale prior values overwrote the new VM: id=%q region=%q", dep.ProviderInstanceID, dep.Region)
		}
	})

	t.Run("ssh over an ssh prior stays ssh", func(t *testing.T) {
		dep := &deploy.Deployment{Provider: deploy.ProviderSSH}
		carryForwardProvisioning(dep, &deploy.Deployment{Provider: deploy.ProviderSSH}, deploy.ProviderSSH)

		if dep.Provider != deploy.ProviderSSH || dep.ProviderInstanceID != "" {
			t.Errorf("BYO-SSH record gained provisioning metadata: %+v", dep)
		}
	})

	t.Run("no prior is a no-op", func(t *testing.T) {
		dep := &deploy.Deployment{Provider: deploy.ProviderSSH}
		carryForwardProvisioning(dep, nil, deploy.ProviderSSH)

		if dep.Provider != deploy.ProviderSSH {
			t.Errorf("Provider = %q, want ssh", dep.Provider)
		}
	})

	t.Run("region already set by the deploy is not overwritten", func(t *testing.T) {
		dep := &deploy.Deployment{Provider: deploy.ProviderSSH, Region: "ams3"}
		carryForwardProvisioning(dep, managedPrior(), deploy.ProviderSSH)

		if dep.Region != "ams3" {
			t.Errorf("Region = %q, want ams3", dep.Region)
		}
	})
}
