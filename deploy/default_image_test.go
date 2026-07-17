package deploy

import (
	"strings"
	"testing"
)

// Every managed provider must default to the SAME Ubuntu LTS. The bootstrap is one
// provider-agnostic script — it adds Docker's and PGDG's apt repos keyed on the
// release codename, and installs the same packages everywhere. A provider drifting
// onto a different release means the bootstrap is only really tested on whichever
// release the last E2E happened to use, and breakage shows up as a failed deploy on
// one provider only.
//
// The values are pinned as literals deliberately: each was checked against the
// provider's live CLI (see the per-provider comments), so a "harmless" edit to one
// constant has to come here and state which release the fleet is on.
func TestDefaultImagesAreAllTheSameUbuntuLTS(t *testing.T) {
	// The 24.04-vs-26.04 call: 24.04 is the newest Ubuntu with a stable identifier on
	// EVERY provider — `az vm image list` has no alias past Ubuntu2404, so 26.04 would
	// force a raw URN there and break the uniform shape. Revisit when Azure adds one.
	const wantRelease = "24.04"

	// Scaleway and Vultr are absent on purpose: they don't name images by version
	// number, so the digit check below can't see the release. Each has its own
	// dedicated assertion.
	tests := []struct {
		provider string
		image    string
	}{
		{"digitalocean", doDefaultImage}, // verified: doctl compute image list-distribution
		{"hetzner", hetznerDefaultImage}, // verified: hcloud image list
		{"linode", linodeDefaultImage},   // verified: linode-cli images list + a real deploy
		{"azure", azureDefaultImage},     // verified: az vm image list
		{"gcp", gcpDefaultImageFamily},   // NOT verified (needs auth) — confirm at first GCP E2E
	}

	// Each provider spells the same release differently, so compare on digits only:
	// 24.04 -> "2404" matches ubuntu-24-04-x64, ubuntu24.04, Ubuntu2404, ubuntu-2404-lts.
	wantDigits := strings.ReplaceAll(wantRelease, ".", "")
	for _, tc := range tests {
		got := digitsOf(tc.image)
		if !strings.Contains(got, wantDigits) {
			t.Errorf("%s defaults to %q, which is not Ubuntu %s — the fleet must stay on one release",
				tc.provider, tc.image, wantRelease)
		}
	}

	// Vultr is the exception: its id is opaque ("2284"), so the digits check above
	// can't see the release. It's pinned separately and must be stated explicitly.
	if vultrDefaultOS != "2284" {
		t.Errorf("vultrDefaultOS = %q, want 2284 (Ubuntu %s x64, per `vultr-cli os list`). "+
			"22.04 = 1743, 26.04 = 2760 — an id says nothing about what it is, so verify before changing",
			vultrDefaultOS, wantRelease)
	}
}

// digitsOf reduces an image identifier to just its digits, so the release can be
// compared across each provider's naming scheme.
func digitsOf(s string) string {
	var b strings.Builder
	for _, r := range s {
		if r >= '0' && r <= '9' {
			b.WriteRune(r)
		}
	}
	return b.String()
}

// Scaleway names images by codename, not version number, so the check above cannot
// catch a wrong-but-plausible codename. Pin it: noble IS 24.04 (jammy was 22.04).
func TestScalewayImageIsTheRightCodename(t *testing.T) {
	if scalewayDefaultImage != "ubuntu_noble" {
		t.Errorf("scalewayDefaultImage = %q, want ubuntu_noble (Ubuntu 24.04; jammy = 22.04)",
			scalewayDefaultImage)
	}
}
