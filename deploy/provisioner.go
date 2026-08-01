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
		string(ProviderLinode),
		string(ProviderGCP),
		string(ProviderAzure),
		string(ProviderVultr),
		string(ProviderScaleway),
	}
}

// plannedProviders are recognized names whose adapter isn't shipped yet — they
// get a "coming soon, use ssh for now" error rather than "unknown provider".
var plannedProviders = map[Provider]bool{
	ProviderAWS: true,
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
	case ProviderLinode:
		return NewLinodeProvisioner(), nil
	case ProviderGCP:
		return NewGCPProvisioner(), nil
	case ProviderAzure:
		return NewAzureProvisioner(), nil
	case ProviderVultr:
		return NewVultrProvisioner(), nil
	case ProviderScaleway:
		return NewScalewayProvisioner(), nil
	}
	if plannedProviders[provider] {
		return nil, fmt.Errorf(
			"provider %q is planned but not available yet — for now create the VM yourself and deploy with --provider ssh --host <ip>, or use one of: %s",
			provider, strings.Join(implementedProviders(), ", "))
	}
	return nil, fmt.Errorf("unknown provider %q — supported: %s", provider, strings.Join(implementedProviders(), ", "))
}

// instanceGonePhrases are how the provider CLIs say "that instance does not exist".
// Matched case-insensitively against the whole error, because every adapter wraps its
// CLI's stderr rather than parsing it.
// Deliberately NOT a bare "not found": `exec: "doctl": executable file not found in
// $PATH` is a missing CLI, which is the opposite of a VM that no longer exists.
var instanceGonePhrases = []string{
	"could not be found", // doctl: "404 ... The resource you were accessing could not be found"
	"was not found",      // gcloud, az
	"server not found",   // hcloud
	"instance not found", // linode-cli, vultr-cli
	"droplet not found",
	"resource not found",
	"notfound",       // az's (ResourceNotFound) and other ARM error codes
	"does not exist", // gcloud
	"no such server", // scaleway
	" 404 ",          // the raw status, as the CLIs print it mid-sentence
	"404:",
	"http 410", // Gone
}

// InstanceAlreadyGone reports whether a failed delete failed because the instance is
// not there any more.
//
// It is the difference between two very different closing messages. Destroying a
// record whose droplet had been deleted underneath it printed a DigitalOcean "404 …
// could not be found" and then told the user to "delete it manually to avoid charges"
// — sending them to hunt for a server that does not exist, on a bill it is not on.
// The "already gone" case is one the CLI recognises everywhere else; only the delete
// path lacked it.
//
// Deliberately a phrase match: the adapters shell out to seven different provider
// CLIs, none of which offers a typed error, and the alternative is seven bespoke
// parsers. A false positive costs the reassurance that a VM is gone when it is not,
// so the phrases stay narrow and the caller still says which provider said it.
func InstanceAlreadyGone(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	for _, phrase := range instanceGonePhrases {
		if strings.Contains(msg, phrase) {
			return true
		}
	}
	return false
}
