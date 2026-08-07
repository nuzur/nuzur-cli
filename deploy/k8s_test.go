package deploy

import (
	"context"
	"fmt"
	"strings"
	"testing"
)

// captureRunner is a RemoteRunner that only answers Capture, from a table of
// command prefixes it considers "working". Every other method panics naming
// itself: an unscripted call is a test bug, and a silent zero value would let a
// detection test pass without detecting anything.
type captureRunner struct {
	works    []string // command prefixes that exit 0
	captured []string // every command probed, in order
}

func (r *captureRunner) Capture(ctx context.Context, command string) (string, error) {
	r.captured = append(r.captured, command)
	for _, ok := range r.works {
		if strings.HasPrefix(command, ok) {
			return "ok", nil
		}
	}
	return "", fmt.Errorf("command not found: %s", command)
}

func (r *captureRunner) Ping(ctx context.Context) error { panic("captureRunner.Ping") }
func (r *captureRunner) RunCommand(ctx context.Context, command string) error {
	panic("captureRunner.RunCommand")
}
func (r *captureRunner) RunScript(ctx context.Context, label, script string) error {
	panic("captureRunner.RunScript")
}
func (r *captureRunner) CopyDir(ctx context.Context, localDir, remotePath string) error {
	panic("captureRunner.CopyDir")
}
func (r *captureRunner) SetSudo(sudo bool) { panic("captureRunner.SetSudo") }

var _ RemoteRunner = (*captureRunner)(nil)

// TestDetectClusterToolsPicksTheRightFamily covers the microk8s-vs-plain
// distinction. There is no flag for it: a microk8s box is recognised by the
// tooling actually answering on the host.
func TestDetectClusterToolsPicksTheRightFamily(t *testing.T) {
	for _, tc := range []struct {
		name        string
		works       []string
		wantHelm    string
		wantKubectl string
		wantMicro   bool
	}{
		{
			name:        "microk8s",
			works:       []string{"microk8s helm3", "microk8s kubectl"},
			wantHelm:    "microk8s helm3",
			wantKubectl: "microk8s kubectl",
			wantMicro:   true,
		},
		{
			name:        "plain k8s",
			works:       []string{"helm ", "kubectl "},
			wantHelm:    "helm",
			wantKubectl: "kubectl",
			wantMicro:   false,
		},
		{
			// An older microk8s exposing `helm` rather than `helm3`.
			name:        "microk8s without helm3",
			works:       []string{"microk8s helm ", "microk8s kubectl"},
			wantHelm:    "microk8s helm",
			wantKubectl: "microk8s kubectl",
			wantMicro:   true,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := &captureRunner{works: tc.works}
			got, err := DetectClusterTools(context.Background(), r, "", "")
			if err != nil {
				t.Fatalf("DetectClusterTools: %v", err)
			}
			if got.Helm != tc.wantHelm || got.Kubectl != tc.wantKubectl {
				t.Errorf("got helm=%q kubectl=%q, want helm=%q kubectl=%q",
					got.Helm, got.Kubectl, tc.wantHelm, tc.wantKubectl)
			}
			if got.IsMicroK8s() != tc.wantMicro {
				t.Errorf("IsMicroK8s() = %v, want %v", got.IsMicroK8s(), tc.wantMicro)
			}
		})
	}
}

// TestDetectClusterToolsPrefersMicroK8sWhenBothExist pins the ordering. A
// microk8s box very often also has a standalone helm/kubectl whose kubeconfig
// points somewhere else entirely — deploying there silently would be worse than
// failing outright.
func TestDetectClusterToolsPrefersMicroK8sWhenBothExist(t *testing.T) {
	r := &captureRunner{works: []string{"microk8s helm3", "microk8s kubectl", "helm ", "kubectl "}}
	got, err := DetectClusterTools(context.Background(), r, "", "")
	if err != nil {
		t.Fatalf("DetectClusterTools: %v", err)
	}
	if !got.IsMicroK8s() {
		t.Errorf("expected microk8s to win when both are present, got helm=%q", got.Helm)
	}
}

// TestDetectClusterToolsPairsHelmAndKubectl is the reason candidates are probed
// as pairs. With microk8s helm but only a standalone kubectl, resolving the two
// independently would install the release through microk8s and then read the
// service address back from whatever cluster plain kubectl points at.
func TestDetectClusterToolsPairsHelmAndKubectl(t *testing.T) {
	r := &captureRunner{works: []string{"microk8s helm3", "kubectl "}}
	_, err := DetectClusterTools(context.Background(), r, "", "")
	if err == nil {
		t.Fatal("expected failure: microk8s helm with no microk8s kubectl is a mismatched pair")
	}
	if !strings.Contains(err.Error(), "no working helm+kubectl pair") {
		t.Errorf("error should explain the pairing requirement, got: %v", err)
	}
}

// TestDetectClusterToolsHonoursOverrides covers the box that runs microk8s but
// where this deploy targets a different cluster.
func TestDetectClusterToolsHonoursOverrides(t *testing.T) {
	r := &captureRunner{works: []string{"microk8s helm3", "microk8s kubectl", "helm ", "kubectl "}}
	got, err := DetectClusterTools(context.Background(), r, "helm", "kubectl")
	if err != nil {
		t.Fatalf("DetectClusterTools: %v", err)
	}
	if got.IsMicroK8s() {
		t.Errorf("explicit override should beat microk8s detection, got helm=%q", got.Helm)
	}
	// An override still gets verified rather than trusted.
	for _, want := range []string{"helm version --short", "kubectl version --client"} {
		var seen bool
		for _, c := range r.captured {
			if c == want {
				seen = true
			}
		}
		if !seen {
			t.Errorf("override %q was never verified; probed: %v", want, r.captured)
		}
	}
}

// TestDetectClusterToolsRejectsABrokenOverride: a typo must fail before the
// chart is copied, not during helm upgrade.
func TestDetectClusterToolsRejectsABrokenOverride(t *testing.T) {
	r := &captureRunner{works: []string{"microk8s helm3", "microk8s kubectl"}}
	_, err := DetectClusterTools(context.Background(), r, "helmm", "")
	if err == nil {
		t.Fatal("expected a broken helm override to fail")
	}
	if !strings.Contains(err.Error(), "helmm") {
		t.Errorf("error should name the failing command, got: %v", err)
	}
}

// TestDetectClusterToolsExplainsAnEmptyHost: the message has to be actionable —
// the most common cause is the ssh user not being in the microk8s group.
func TestDetectClusterToolsExplainsAnEmptyHost(t *testing.T) {
	r := &captureRunner{}
	_, err := DetectClusterTools(context.Background(), r, "", "")
	if err == nil {
		t.Fatal("expected failure when nothing is installed")
	}
	for _, want := range []string{"microk8s group", "--helm-cmd"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error should mention %q, got: %v", want, err)
		}
	}
}

// TestCreatesInfrastructureCoversEveryProvider pins the predicate that nine
// call sites now depend on — the pending record, the provider firewall, the
// "Creating the server…" notice, managed-box reuse, destroy's VM delete and
// carry-forward all branch on it. A new provider added to the enum without a
// deliberate answer here silently inherits "creates a VM", which for a BYO
// provider means destroy would try to delete a machine nuzur never made.
func TestCreatesInfrastructureCoversEveryProvider(t *testing.T) {
	want := map[Provider]bool{
		"":                   false, // defaults to BYO-SSH
		ProviderSSH:          false,
		ProviderK8s:          false, // the user's cluster; nuzur creates nothing
		ProviderDigitalOcean: true,
		ProviderHetzner:      true,
		ProviderAWS:          true,
		ProviderGCP:          true,
		ProviderAzure:        true,
		ProviderVultr:        true,
		ProviderLinode:       true,
		ProviderScaleway:     true,
	}
	for provider, expected := range want {
		if got := provider.CreatesInfrastructure(); got != expected {
			t.Errorf("%q.CreatesInfrastructure() = %v, want %v", provider, got, expected)
		}
		if provider.UsesGivenHost() == provider.CreatesInfrastructure() {
			t.Errorf("%q: UsesGivenHost must be the inverse of CreatesInfrastructure", provider)
		}
	}
}

// TestK8sProvisionerRequiresAHost keeps the k8s provider from silently
// defaulting to some other machine.
func TestK8sProvisionerRequiresAHost(t *testing.T) {
	_, err := NewK8sProvisioner().Provision(context.Background(), Spec{})
	if err == nil {
		t.Fatal("expected --host to be required")
	}
	if !strings.Contains(err.Error(), "--host") {
		t.Errorf("error should name the missing flag, got: %v", err)
	}
}

// TestK8sProvisionerCreatesNothing: every provider-side operation is a no-op,
// because the user owns the cluster. In particular ConfigureFirewall must do
// nothing — the VM path's ufw rules would sever a Kubernetes node's API server,
// kubelet and NodePort range.
func TestK8sProvisionerCreatesNothing(t *testing.T) {
	p := NewK8sProvisioner()
	prov, err := p.Provision(context.Background(), Spec{Target: Target{Host: "203.0.113.10"}})
	if err != nil {
		t.Fatalf("Provision: %v", err)
	}
	if prov.InstanceID != "" || prov.Region != "" {
		t.Errorf("k8s provider must claim no provider-side instance, got %+v", prov)
	}
	if err := p.ConfigureFirewall(context.Background(), prov, []FirewallRule{{Port: 80}}); err != nil {
		t.Errorf("ConfigureFirewall must be a no-op: %v", err)
	}
	if err := p.Destroy(context.Background(), prov); err != nil {
		t.Errorf("Destroy must be a no-op: %v", err)
	}
	id, err := p.FindInstanceByName(context.Background(), "anything", "")
	if err != nil || id != "" {
		t.Errorf("FindInstanceByName should find nothing, got %q, %v", id, err)
	}
}
