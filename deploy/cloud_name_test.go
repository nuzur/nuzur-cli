package deploy

import (
	"context"
	"strings"
	"testing"
)

// The whole point of the random tail: two deploys of the SAME identifier must not
// produce the same provider-side name, or they'd collide with each other (and
// with anything the user already named "nuzur-<identifier>").
func TestProviderResourceNameIsUnique(t *testing.T) {
	seen := map[string]bool{}
	for i := 0; i < 5; i++ {
		got, err := providerResourceName("sfapi")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if !strings.HasPrefix(got, "nuzur-sfapi-") {
			t.Errorf("name = %q, want it to keep the readable nuzur-sfapi- prefix", got)
		}
		if seen[got] {
			t.Fatalf("name %q repeated — a collision with the user's own resources is exactly what this prevents", got)
		}
		seen[got] = true
	}
}

func TestProviderResourceNameSanitizesAndBounds(t *testing.T) {
	origSuffix := providerNameSuffix
	providerNameSuffix = func() (string, error) { return "abc123", nil }
	t.Cleanup(func() { providerNameSuffix = origSuffix })

	cases := []struct {
		identifier string
		want       string
		why        string
	}{
		{"sfapi", "nuzur-sfapi-abc123", "plain identifier"},
		{"My App", "nuzur-my-app-abc123", "spaces and case are not portable across providers"},
		{"a_b.c", "nuzur-a-b-c-abc123", "underscores/dots collapse to dashes"},
		{"--weird--", "nuzur-weird-abc123", "leading/trailing/repeated separators are trimmed"},
		{"", "nuzur-app-abc123", "an empty identifier still yields a valid name"},
	}
	for _, tc := range cases {
		got, err := providerResourceName(tc.identifier)
		if err != nil {
			t.Fatalf("%s: unexpected error: %v", tc.why, err)
		}
		if got != tc.want {
			t.Errorf("%s: providerResourceName(%q) = %q, want %q", tc.why, tc.identifier, got, tc.want)
		}
	}

	// Linode caps labels at 32 chars — the tightest provider — so a long
	// identifier must be truncated rather than rejected at create.
	long, err := providerResourceName(strings.Repeat("verylongname", 5))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(long) > providerNameMaxLen {
		t.Errorf("name %q is %d chars, want <= %d", long, len(long), providerNameMaxLen)
	}
	if strings.Contains(long, "--") || strings.HasSuffix(long, "-abc123") == false {
		t.Errorf("truncated name %q should stay well-formed and keep the suffix", long)
	}
}

// `az group create` is an idempotent ARM PUT: on an existing group it SUCCEEDS
// and hands it back. Since Destroy deletes the whole group, adopting one the user
// already had would destroy their resources — so an existing group must hard-fail.
func TestAzureRefusesExistingResourceGroup(t *testing.T) {
	calls := stubCLI(t, func(name string, args []string) (string, error) {
		if findArg(args, "group") && findArg(args, "exists") {
			return "true", nil // someone already owns this group
		}
		if findArg(args, "vm") && findArg(args, "create") {
			return "20.51.1.9", nil
		}
		return "", nil
	})

	_, err := NewAzureProvisioner().Provision(context.Background(), Spec{
		Identifier:     "sfapi",
		ProviderConfig: ProviderConfig{Region: "eastus"},
		Target:         Target{KeyPath: testPubKeyPath(t)},
	})
	if err == nil || !strings.Contains(err.Error(), "already exists") {
		t.Fatalf("err = %v, want a refusal to deploy into an existing resource group", err)
	}
	// And crucially: it must not have created or touched anything.
	if findCall(*calls, "group", "create") != nil {
		t.Error("must not create/adopt a resource group that already exists")
	}
	if findCall(*calls, "vm", "create") != nil {
		t.Error("must not create a VM inside a group nuzur doesn't own")
	}
}
