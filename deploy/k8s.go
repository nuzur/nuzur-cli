package deploy

import (
	"context"
	"fmt"
	"strings"
)

// ─────────────────────────────────────────────
// Kubernetes provisioner
// ─────────────────────────────────────────────

// K8sProvisioner implements Provisioner for an existing Kubernetes cluster
// reached over SSH. Like SSHProvisioner it does no provider-API work: the
// cluster already exists and the user owns it.
//
// The Provisioner seam is a poor fit here — it models "get me an SSH-reachable
// box", and for this provider that box is only a place to run helm from. The
// actual work (chart, image, release) lives in the deploy pipeline's k8s steps.
// Implementing it anyway keeps one registry of providers and one code path for
// resolving a target.
type K8sProvisioner struct{}

func NewK8sProvisioner() *K8sProvisioner { return &K8sProvisioner{} }

func (p *K8sProvisioner) Provision(ctx context.Context, spec Spec) (Provisioned, error) {
	t := spec.Target
	if strings.TrimSpace(t.Host) == "" {
		return Provisioned{}, fmt.Errorf("--host is required for the k8s provider (the machine that can reach your cluster)")
	}
	if t.User == "" {
		t.User = "root"
	}
	if t.Port == 0 {
		t.Port = 22
	}
	return Provisioned{Target: t}, nil
}

// ConfigureFirewall is a no-op. Cluster networking is the cluster's business,
// and the ufw rules the VM path applies would break a Kubernetes node.
func (p *K8sProvisioner) ConfigureFirewall(ctx context.Context, prov Provisioned, rules []FirewallRule) error {
	return nil
}

// Destroy is a no-op: the user owns the cluster. Removing the release is the
// destroy command's job (helm uninstall), not the provisioner's.
func (p *K8sProvisioner) Destroy(ctx context.Context, prov Provisioned) error { return nil }

// FindInstanceByName finds nothing: nuzur created no instance.
func (p *K8sProvisioner) FindInstanceByName(ctx context.Context, name, region string) (string, error) {
	return "", nil
}

// ─────────────────────────────────────────────
// Cluster tooling, resolved on the box
// ─────────────────────────────────────────────

// ClusterTools are the helm and kubectl entrypoints to use on a given host.
//
// They are full command prefixes rather than binary names because microk8s
// bundles its own: `microk8s helm3` and `microk8s kubectl`, which read the
// cluster's config without a kubeconfig existing anywhere. A host with a
// conventional install gets plain `helm` / `kubectl` instead.
type ClusterTools struct {
	Helm    string
	Kubectl string
}

// toolCandidates are probed in order, as MATCHED PAIRS.
//
// Pairing is the point. Resolving helm and kubectl independently on a box that
// has both microk8s and a standalone kubectl can pick `microk8s helm3` for the
// install and a plain `kubectl` — pointed at whatever ~/.kube/config says — for
// the readback. Those can be different clusters, so the release lands in one and
// we report the address of another. Whichever family answers first supplies both.
//
// microk8s comes first deliberately: on a microk8s box a bare `helm` often also
// exists but talks to a different (or no) cluster, and silently deploying
// somewhere other than the cluster the user meant is worse than not deploying.
// Pass explicit commands to override the order entirely.
var toolCandidates = []ClusterTools{
	{Helm: "microk8s helm3", Kubectl: "microk8s kubectl"},
	{Helm: "microk8s helm", Kubectl: "microk8s kubectl"},
	{Helm: "helm", Kubectl: "kubectl"},
}

// DetectClusterTools resolves the helm and kubectl entrypoints on the host.
//
// helmOverride/kubectlOverride short-circuit the probe — for a host that runs
// microk8s but where the deploy targets some other cluster, or any layout the
// candidate list does not cover. Supplying one but not the other overrides just
// that half, which is deliberate: the pairing rule is a default, not a
// constraint on someone who knows their own box.
//
// Candidates are probed by RUNNING them, not by looking for a binary:
// `microk8s helm3` is a subcommand, so `command -v` says nothing about whether
// it works — and an install the ssh user cannot reach (not in the microk8s
// group) fails here, with an actionable message, rather than several steps later
// behind a confusing permission error.
func DetectClusterTools(ctx context.Context, runner RemoteRunner, helmOverride, kubectlOverride string) (ClusterTools, error) {
	tools := ClusterTools{
		Helm:    strings.TrimSpace(helmOverride),
		Kubectl: strings.TrimSpace(kubectlOverride),
	}

	if tools.Helm == "" || tools.Kubectl == "" {
		detected, err := probeClusterTools(ctx, runner)
		if err != nil {
			return ClusterTools{}, err
		}
		if tools.Helm == "" {
			tools.Helm = detected.Helm
		}
		if tools.Kubectl == "" {
			tools.Kubectl = detected.Kubectl
		}
	}

	// An override is still verified — a typo here would otherwise surface as a
	// failed helm upgrade after the chart has been copied.
	if _, err := runner.Capture(ctx, tools.Helm+" version --short"); err != nil {
		return ClusterTools{}, fmt.Errorf("helm command %q does not work on the host: %w", tools.Helm, err)
	}
	if _, err := runner.Capture(ctx, tools.Kubectl+" version --client"); err != nil {
		return ClusterTools{}, fmt.Errorf("kubectl command %q does not work on the host: %w", tools.Kubectl, err)
	}

	return tools, nil
}

// probeClusterTools returns the first candidate pair whose helm AND kubectl both
// work, so the two always address the same cluster.
func probeClusterTools(ctx context.Context, runner RemoteRunner) (ClusterTools, error) {
	for _, candidate := range toolCandidates {
		if _, err := runner.Capture(ctx, candidate.Helm+" version --short"); err != nil {
			continue
		}
		if _, err := runner.Capture(ctx, candidate.Kubectl+" version --client"); err != nil {
			continue
		}
		return candidate, nil
	}

	var tried []string
	for _, candidate := range toolCandidates {
		tried = append(tried, candidate.Helm+" / "+candidate.Kubectl)
	}
	return ClusterTools{}, fmt.Errorf(
		"no working helm+kubectl pair found on the host — tried: %s.\n"+
			"Install helm and kubectl, or on microk8s add the ssh user to the microk8s group (`sudo usermod -aG microk8s <user>` then reconnect).\n"+
			"To use a specific pair, pass --helm-cmd and --kubectl-cmd",
		strings.Join(tried, "; "))
}

// IsMicroK8s reports whether the resolved tooling is microk8s. Callers use it
// for guidance that only makes sense there, such as naming the addons a feature
// needs (`microk8s enable ingress`).
func (t ClusterTools) IsMicroK8s() bool {
	return strings.HasPrefix(t.Helm, "microk8s ")
}

// ReachCluster verifies the resolved tooling can actually talk to a cluster.
//
// Separate from DetectClusterTools because "helm is installed" and "helm can
// reach a cluster" fail for different reasons and want different messages. This
// runs before anything is copied or built, so an unreachable cluster costs
// nothing.
func (t ClusterTools) ReachCluster(ctx context.Context, runner RemoteRunner) error {
	if _, err := runner.Capture(ctx, t.Kubectl+" get nodes -o name"); err != nil {
		return fmt.Errorf("cannot reach the cluster with %q: %w", t.Kubectl, err)
	}
	return nil
}
