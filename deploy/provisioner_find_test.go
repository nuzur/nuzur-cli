package deploy

import (
	"context"
	"testing"
)

// FindInstanceByName is the recovery path for a deploy killed DURING the create
// call: the id never came back, so the name minted beforehand is the only handle on
// a VM that may be running and billing. It feeds straight into a delete, which
// makes both of its failure modes expensive — missing the VM leaks it, and matching
// the WRONG one destroys something that isn't ours.

// findCase scripts one adapter's list output: a VM named ours, a decoy whose name
// merely starts with ours, and an unrelated box.
type findCase struct {
	provider string
	newP     func() Provisioner
	// list output when the account has our VM (plus neighbours).
	populated string
	wantID    string
}

func findCases() []findCase {
	return []findCase{
		{
			provider: "linode", newP: func() Provisioner { return NewLinodeProvisioner() },
			populated: "4711 nuzur-sfapi-abc123\n4712 nuzur-sfapi-abc123-old\n62189476 nuzur",
			wantID:    "4711",
		},
		{
			provider: "digitalocean", newP: func() Provisioner { return NewDigitalOceanProvisioner() },
			populated: "12345 nuzur-sfapi-abc123\n12346 nuzur-sfapi-abc123-old\n999 my-blog",
			wantID:    "12345",
		},
		{
			provider: "hetzner", newP: func() Provisioner { return NewHetznerProvisioner() },
			populated: "777 nuzur-sfapi-abc123\n778 nuzur-sfapi-abc123-old\n1 web",
			wantID:    "777",
		},
		{
			provider: "scaleway", newP: func() Provisioner { return NewScalewayProvisioner() },
			// Scaleway's name= filter is a PREFIX match server-side, so the decoy is
			// genuinely returned by the CLI and the adapter has to reject it.
			populated: "sc-1 nuzur-sfapi-abc123\nsc-2 nuzur-sfapi-abc123-old",
			wantID:    "sc-1",
		},
	}
}

func TestFindInstanceByNameMatchesExactlyOrNotAtAll(t *testing.T) {
	const want = "nuzur-sfapi-abc123"
	for _, tc := range findCases() {
		t.Run(tc.provider, func(t *testing.T) {
			// Found: the exact name wins over the prefix decoy sharing its start.
			stubCLI(t, func(name string, args []string) (string, error) { return tc.populated, nil })
			got, err := tc.newP().FindInstanceByName(context.Background(), want, regionFor(tc.provider))
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got != tc.wantID {
				t.Errorf("FindInstanceByName = %q, want %q — the wrong id here deletes the wrong server", got, tc.wantID)
			}

			// Not found: an empty account is a normal outcome (the create never took
			// effect), not an error — erroring would abort a destroy that has nothing
			// left to do.
			stubCLI(t, func(name string, args []string) (string, error) { return "", nil })
			got, err = tc.newP().FindInstanceByName(context.Background(), want, regionFor(tc.provider))
			if err != nil {
				t.Errorf("no match should not be an error, got %v", err)
			}
			if got != "" {
				t.Errorf("FindInstanceByName = %q, want \"\" when nothing matches", got)
			}

			// Only a near-miss exists: must NOT be adopted.
			stubCLI(t, func(name string, args []string) (string, error) {
				return "9999 " + want + "-old", nil
			})
			got, _ = tc.newP().FindInstanceByName(context.Background(), want, regionFor(tc.provider))
			if got != "" {
				t.Errorf("FindInstanceByName matched %q on a name that only starts with ours — that deletes someone else's server", got)
			}

			// An empty name must never be resolved: it would match anything.
			got, _ = tc.newP().FindInstanceByName(context.Background(), "", regionFor(tc.provider))
			if got != "" {
				t.Errorf("an empty name resolved to %q; it must match nothing", got)
			}
		})
	}
}

// Azure's "instance" is the resource group, so its lookup is an existence check
// rather than a list — `az group exists` prints true/false.
func TestAzureFindInstanceByNameUsesGroupExists(t *testing.T) {
	calls := stubCLI(t, func(name string, args []string) (string, error) {
		if findArg(args, "group") && findArg(args, "exists") {
			return "true", nil
		}
		return "", nil
	})
	got, err := NewAzureProvisioner().FindInstanceByName(context.Background(), "nuzur-sfapi-abc123", "eastus")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != "nuzur-sfapi-abc123" {
		t.Errorf("got %q, want the resource group name (Azure's instance id)", got)
	}
	if findCall(*calls, "group", "exists", "--name", "nuzur-sfapi-abc123") == nil {
		t.Errorf("expected `az group exists`; calls: %+v", *calls)
	}

	stubCLI(t, func(name string, args []string) (string, error) { return "false", nil })
	got, err = NewAzureProvisioner().FindInstanceByName(context.Background(), "nuzur-sfapi-abc123", "eastus")
	if err != nil || got != "" {
		t.Errorf("got (%q, %v), want (\"\", nil) when the group does not exist", got, err)
	}
}

// GCP addresses instances by name, so a hit returns the name itself.
func TestGCPFindInstanceByNameIsZoneScoped(t *testing.T) {
	calls := stubCLI(t, func(name string, args []string) (string, error) {
		return "nuzur-sfapi-abc123", nil
	})
	got, err := NewGCPProvisioner().FindInstanceByName(context.Background(), "nuzur-sfapi-abc123", "us-central1-b")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != "nuzur-sfapi-abc123" {
		t.Errorf("got %q, want the instance name (GCP's instance id)", got)
	}
	// Without the zone gcloud searches every zone, which is both slow and able to
	// return an instance from a zone this deployment never used.
	if findCall(*calls, "--zones", "us-central1-b") == nil {
		t.Errorf("lookup should be scoped to the deployment's zone; calls: %+v", *calls)
	}
}

// Vultr has no server-side label filter, so the match happens on parsed JSON.
func TestVultrFindInstanceByNameMatchesLabel(t *testing.T) {
	stubCLI(t, func(name string, args []string) (string, error) {
		return `{"instances":[{"id":"v-1","label":"nuzur-sfapi-abc123-old"},{"id":"v-9","label":"nuzur-sfapi-abc123"}]}`, nil
	})
	got, err := NewVultrProvisioner().FindInstanceByName(context.Background(), "nuzur-sfapi-abc123", "ewr")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != "v-9" {
		t.Errorf("got %q, want v-9 — the exact label, not the one that merely starts with it", got)
	}
}

// BYO-SSH created nothing, so it must never claim to find one of ours.
func TestSSHFindInstanceByNameFindsNothing(t *testing.T) {
	got, err := NewSSHProvisioner().FindInstanceByName(context.Background(), "anything", "")
	if err != nil || got != "" {
		t.Errorf("got (%q, %v), want (\"\", nil) — BYO-SSH owns no instances", got, err)
	}
}
