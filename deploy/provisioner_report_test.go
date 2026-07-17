package deploy

import (
	"context"
	"strings"
	"testing"
	"time"
)

// Creating a VM is a side effect that cannot be made atomic with recording it, so
// the deploy records what it knows as early as it can. These tests pin the two
// halves of that contract, which is the difference between an interrupted deploy
// being recoverable and silently leaving a server running and billing forever:
//
//  1. every managed adapter reports the instance BEFORE it waits for SSH — that
//     wait is minutes long and used to be a window with nothing on disk; and
//  2. every adapter uses the name the CALLER minted, because that name is written
//     to local state before the create call and is the only handle on a VM whose
//     id never came back.
//
// BYO-SSH is excluded: nuzur creates nothing, so there is nothing to report.

// provisionCase is one adapter's happy path.
type provisionCase struct {
	provider   string
	newP       func() Provisioner
	handler    func(t *testing.T) func(name string, args []string) (string, error)
	wantID     string // instance id the scripted create returns
	wantHostIn bool   // whether the adapter can know the IP at report time
}

func provisionCases() []provisionCase {
	return []provisionCase{
		{
			provider: "linode", newP: func() Provisioner { return NewLinodeProvisioner() },
			handler: linodeHandler, wantID: "4711", wantHostIn: true,
		},
		{
			provider: "digitalocean", newP: func() Provisioner { return NewDigitalOceanProvisioner() },
			handler: func(t *testing.T) func(string, []string) (string, error) {
				return func(name string, args []string) (string, error) {
					if findArg(args, "droplet") && findArg(args, "create") {
						return "12345  159.203.1.1", nil
					}
					return "", nil
				}
			}, wantID: "12345", wantHostIn: true,
		},
		{
			provider: "hetzner", newP: func() Provisioner { return NewHetznerProvisioner() },
			handler: func(t *testing.T) func(string, []string) (string, error) {
				return func(name string, args []string) (string, error) {
					switch {
					case findArg(args, "server") && findArg(args, "describe"):
						return "777", nil
					case findArg(args, "server") && findArg(args, "ip"):
						return "49.12.1.1", nil
					case findArg(args, "ssh-key") && findArg(args, "list"):
						return "nuzur-deploy", nil
					}
					return "", nil
				}
			}, wantID: "777", wantHostIn: true,
		},
		{
			provider: "gcp", newP: func() Provisioner { return NewGCPProvisioner() },
			handler: func(t *testing.T) func(string, []string) (string, error) { return gcpHandler() },
			// GCP addresses instances by name, so the "id" IS the minted name.
			wantID: "nuzur-sfapi-abc123", wantHostIn: true,
		},
		{
			provider: "azure", newP: func() Provisioner { return NewAzureProvisioner() },
			handler: func(t *testing.T) func(string, []string) (string, error) { return azureHandler() },
			// Azure's id is the resource group, reported before the VM even exists —
			// so there is no IP to report yet.
			wantID: "nuzur-sfapi-abc123", wantHostIn: false,
		},
		{
			provider: "scaleway", newP: func() Provisioner { return NewScalewayProvisioner() },
			handler: func(t *testing.T) func(string, []string) (string, error) {
				return func(name string, args []string) (string, error) {
					switch {
					case findArg(args, "server") && findArg(args, "create"):
						return "sc-1 51.15.1.1", nil
					case findArg(args, "ssh-key") && findArg(args, "list"):
						return "key-1", nil
					}
					return "", nil
				}
			}, wantID: "sc-1", wantHostIn: true,
		},
		{
			provider: "vultr", newP: func() Provisioner { return NewVultrProvisioner() },
			handler: func(t *testing.T) func(string, []string) (string, error) {
				return func(name string, args []string) (string, error) {
					switch {
					case findArg(args, "instance") && findArg(args, "create"):
						return `{"instance":{"id":"v-9","main_ip":"0.0.0.0"}}`, nil
					case findArg(args, "instance") && findArg(args, "get"):
						return `{"instance":{"id":"v-9","main_ip":"45.32.1.1"}}`, nil
					case findArg(args, "ssh-key") && findArg(args, "list"):
						return `{"ssh_keys":[{"id":"k-1","name":"nuzur-deploy"}]}`, nil
					}
					return "", nil
				}
			},
			// Vultr reports 0.0.0.0 until it assigns an address, so the IP is not yet
			// knowable at report time — the id is, and that is what deletes it.
			wantID: "v-9", wantHostIn: false,
		},
	}
}

// The load-bearing test: the instance must be handed back before the SSH wait, not
// after it. Reporting after would leave the whole multi-minute wait uncovered —
// which is exactly the gap that leaked a live VM.
func TestProvisionReportsInstanceBeforeWaitingForSSH(t *testing.T) {
	for _, tc := range provisionCases() {
		t.Run(tc.provider, func(t *testing.T) {
			stubCLI(t, tc.handler(t))

			// Track the ordering by watching when the SSH wait starts.
			var sshWaitStarted bool
			origReady := sshReady
			sshReady = func(ctx context.Context, target Target, d time.Duration) error {
				sshWaitStarted = true
				return nil
			}
			t.Cleanup(func() { sshReady = origReady })

			var got []InstanceRef
			var reportedBeforeWait bool
			_, err := tc.newP().Provision(context.Background(), Spec{
				Identifier:     "sfapi",
				ResourceName:   "nuzur-sfapi-abc123",
				ProviderConfig: ProviderConfig{Region: regionFor(tc.provider)},
				Target:         Target{KeyPath: testPubKeyPath(t)},
				OnInstanceCreated: func(ref InstanceRef) {
					if len(got) == 0 {
						reportedBeforeWait = !sshWaitStarted
					}
					got = append(got, ref)
				},
			})
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}

			if len(got) == 0 {
				t.Fatal("the VM was never reported — an interrupted deploy would leave it running with nothing on disk pointing at it")
			}
			if !reportedBeforeWait {
				t.Error("the VM was reported only AFTER the SSH wait started; the whole point is to cover that wait")
			}
			ref := got[0]
			if ref.InstanceID != tc.wantID {
				t.Errorf("reported InstanceID = %q, want %q — a wrong id cannot be deleted", ref.InstanceID, tc.wantID)
			}
			// The name is the fallback handle when the id is unknown; reporting it
			// empty would silently disable that recovery path.
			if ref.ResourceName != "nuzur-sfapi-abc123" {
				t.Errorf("reported ResourceName = %q, want the minted name", ref.ResourceName)
			}
			if tc.wantHostIn && ref.Host == "" {
				t.Errorf("reported no Host although %s knows the address at create time", tc.provider)
			}
		})
	}
}

// The caller mints the name and writes it to disk BEFORE the create call. An
// adapter that minted its own instead would create a VM under a name nothing on
// disk knows — unfindable, and so undeletable, exactly when it matters.
func TestProvisionUsesTheCallerMintedName(t *testing.T) {
	const minted = "nuzur-caller-minted-name"
	for _, tc := range provisionCases() {
		t.Run(tc.provider, func(t *testing.T) {
			calls := stubCLI(t, tc.handler(t))

			var got []InstanceRef
			if _, err := tc.newP().Provision(context.Background(), Spec{
				Identifier:        "sfapi",
				ResourceName:      minted,
				ProviderConfig:    ProviderConfig{Region: regionFor(tc.provider)},
				Target:            Target{KeyPath: testPubKeyPath(t)},
				OnInstanceCreated: func(ref InstanceRef) { got = append(got, ref) },
			}); err != nil {
				t.Fatalf("unexpected error: %v", err)
			}

			// The minted name must reach the provider, not just the report.
			var sawInArgv bool
			for _, c := range *calls {
				if strings.Contains(joined(c.args), minted) {
					sawInArgv = true
					break
				}
			}
			if !sawInArgv {
				t.Errorf("%s never passed the caller's name to its CLI — it minted its own, so state written before the create would name a resource that doesn't exist", tc.provider)
			}
			if len(got) > 0 && got[0].ResourceName != minted {
				t.Errorf("reported ResourceName = %q, want the caller's %q", got[0].ResourceName, minted)
			}
		})
	}
}

// An adapter must still work for a caller that mints nothing (tests, direct use).
func TestProvisionMintsANameWhenTheCallerDidNot(t *testing.T) {
	stubCLI(t, linodeHandler(t))

	var got []InstanceRef
	if _, err := NewLinodeProvisioner().Provision(context.Background(), Spec{
		Identifier:        "sfapi",
		Target:            Target{KeyPath: testPubKeyPath(t)},
		OnInstanceCreated: func(ref InstanceRef) { got = append(got, ref) },
	}); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(got) == 0 || got[0].ResourceName != "nuzur-sfapi-abc123" {
		t.Errorf("with no caller-minted name the adapter should mint one; got %+v", got)
	}
}

// A nil callback is the norm for direct callers and must not panic.
func TestProvisionWithoutACallbackIsFine(t *testing.T) {
	stubCLI(t, linodeHandler(t))
	if _, err := NewLinodeProvisioner().Provision(context.Background(), Spec{
		Identifier: "sfapi",
		Target:     Target{KeyPath: testPubKeyPath(t)},
	}); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

// regionFor gives each adapter a region it accepts (GCP and Scaleway take zones).
func regionFor(provider string) string {
	switch provider {
	case "gcp":
		return gcpDefaultZone
	case "scaleway":
		return scalewayDefaultZone
	case "azure":
		return azureDefaultRegion
	}
	return ""
}
