package deploy

import (
	"fmt"
	"strings"
)

// implementedProviders are the providers with a working adapter today. BYO-SSH
// plus the two Phase-1 cloud adapters; the rest are added incrementally.
func implementedProviders() []string {
	return []string{
		string(ProviderSSH),
		string(ProviderDigitalOcean),
		string(ProviderHetzner),
	}
}

// plannedProviders are recognized names whose adapter isn't shipped yet — they
// get a "coming soon, use ssh for now" error rather than "unknown provider".
var plannedProviders = map[Provider]bool{
	ProviderAWS:      true,
	ProviderGCP:      true,
	ProviderAzure:    true,
	ProviderVultr:    true,
	ProviderLinode:   true,
	ProviderScaleway: true,
}

// NewProvisioner returns the adapter for a provider. An empty provider defaults
// to BYO-SSH.
func NewProvisioner(provider Provider) (Provisioner, error) {
	switch provider {
	case ProviderSSH, "":
		return NewSSHProvisioner(), nil
	case ProviderDigitalOcean:
		return NewDigitalOceanProvisioner(), nil
	case ProviderHetzner:
		return NewHetznerProvisioner(), nil
	}
	if plannedProviders[provider] {
		return nil, fmt.Errorf(
			"provider %q is planned but not available yet — for now create the VM yourself and deploy with --provider ssh --host <ip>, or use one of: %s",
			provider, strings.Join(implementedProviders(), ", "))
	}
	return nil, fmt.Errorf("unknown provider %q — supported: %s", provider, strings.Join(implementedProviders(), ", "))
}
