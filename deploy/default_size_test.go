package deploy

import "testing"

// Every managed provider must default to a shape with at least 2GB of RAM.
//
// The generated Dockerfile compiles the Go app ON THE BOX. On a 1GB shape that
// build exhausts memory: the droplet this was found on went to a few MB available
// with no swap, stopped answering SSH, and had to be killed after 35 minutes —
// having already billed for provisioning, Docker, the database and the image pull,
// because the build is step ~8 of 12. `--size`'s own help states the rule ("a ~2GB
// instance per provider — the app image is built on the box, which OOMs on 1GB");
// DigitalOcean's default silently disagreed with it.
//
// The values are pinned as literals with their memory alongside, because a size
// name says nothing checkable about how much RAM it is: changing one of these
// constants has to come here and state what the new shape actually is.
func TestDefaultSizesAreAtLeast2GB(t *testing.T) {
	const minGB = 2

	for _, tc := range []struct {
		provider string
		got      string
		want     string
		gb       int // RAM of `want`, per the provider's own catalog
	}{
		{"digitalocean", doDefaultSize, "s-2vcpu-2gb", 2}, // s-1vcpu-1gb (1GB) OOMs the build
		{"hetzner", hetznerDefaultType, "cpx22", 8},
		{"linode", linodeDefaultType, "g6-standard-1", 2},
		{"gcp", gcpDefaultMachineType, "e2-small", 2},
		{"azure", azureDefaultSize, "Standard_B2s", 4},
		{"vultr", vultrDefaultPlan, "vc2-1c-2gb", 2},
		{"scaleway", scalewayDefaultType, "DEV1-S", 2},
	} {
		if tc.got != tc.want {
			t.Errorf("%s defaults to %q, want %q (%dGB) — if the shape really changed, update the "+
				"memory alongside it and check it is still at least %dGB",
				tc.provider, tc.got, tc.want, tc.gb, minGB)
		}
		if tc.gb < minGB {
			t.Errorf("%s defaults to %s, which is %dGB — under the %dGB the on-box build needs. "+
				"The deploy dies at step 8 of 12, after the VM, Docker and the database have all billed",
				tc.provider, tc.want, tc.gb, minGB)
		}
	}
}
