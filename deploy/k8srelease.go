package deploy

import (
	"context"
	"fmt"
	"strings"
)

// RemoteChartDir is where the chart is copied on the host before helm runs.
// Under /tmp and namespaced by release so two concurrent deploys of different
// projects cannot overwrite each other's chart mid-upgrade.
func RemoteChartDir(release string) string { return "/tmp/nuzur-chart-" + release }

// RemoteValuesPath is the values file deploy renders on the host.
//
// It carries no secrets — the image reference, chart version and ingress host
// only — because credentials come from the operator-managed file on the node.
// That is also why it can live in /tmp with ordinary permissions.
func RemoteValuesPath(release string) string { return RemoteChartDir(release) + "-values.yaml" }

// ReleaseOptions is one `helm upgrade --install`.
type ReleaseOptions struct {
	Release   string
	Namespace string
	ChartDir  string // path ON THE HOST
	// ValuesFiles are applied in order, so later files win. The generated one
	// comes first and any user --chart-values file last, letting an operator
	// override anything deploy decided.
	ValuesFiles []string
	// Wait blocks until the release's pods are Ready. Without it helm returns as
	// soon as the objects are accepted, and a deploy would report success while
	// the pods are still in ImagePullBackOff.
	Wait        bool
	TimeoutSecs int
}

// UpdateDependencies vendors any subcharts the chart declares.
//
// Required before install or template on a chart with a dependencies block:
// helm refuses outright with "found in Chart.yaml, but missing in charts/
// directory". sfapi declares sfauthserver this way, so without this the release
// fails after the chart has already been copied to the host.
//
// A no-op for a chart with no dependencies, so it is unconditional rather than
// gated on parsing Chart.yaml here.
func (t ClusterTools) UpdateDependencies(ctx context.Context, runner RemoteRunner, chartDir string) error {
	return runner.RunCommand(ctx, strings.Join([]string{
		t.Helm, "dependency", "update", shellQuote(chartDir),
	}, " "))
}

// UpgradeRelease installs or upgrades the release on the host.
//
// `upgrade --install` rather than `install`, so the first deploy and every
// re-deploy are the same command — the property the whole pipeline leans on for
// re-runnability.
func (t ClusterTools) UpgradeRelease(ctx context.Context, runner RemoteRunner, opts ReleaseOptions) error {
	if strings.TrimSpace(opts.Release) == "" || strings.TrimSpace(opts.Namespace) == "" {
		return fmt.Errorf("helm upgrade needs both a release name and a namespace")
	}

	args := []string{
		t.Helm, "upgrade", "--install",
		shellQuote(opts.Release),
		shellQuote(opts.ChartDir),
		"--namespace", shellQuote(opts.Namespace),
		// The namespace is ours to create but never ours to delete: destroy
		// leaves it alone, since anything else in it belongs to the user.
		"--create-namespace",
	}
	for _, f := range opts.ValuesFiles {
		args = append(args, "--values", shellQuote(f))
	}
	if opts.Wait {
		args = append(args, "--wait")
		if opts.TimeoutSecs > 0 {
			args = append(args, "--timeout", fmt.Sprintf("%ds", opts.TimeoutSecs))
		}
	}
	// Roll back a failed upgrade rather than leaving the release wedged
	// half-applied for the next run to puzzle over.
	args = append(args, "--atomic")

	return runner.RunCommand(ctx, strings.Join(args, " "))
}

// UninstallRelease removes the release, leaving the namespace in place.
//
// A missing release is not an error: destroy has to be re-runnable, and a
// record whose release was already removed by hand is a state destroy should
// finish cleaning up rather than refuse.
func (t ClusterTools) UninstallRelease(ctx context.Context, runner RemoteRunner, release, namespace string) error {
	cmd := strings.Join([]string{
		t.Helm, "uninstall", shellQuote(release),
		"--namespace", shellQuote(namespace),
		"--ignore-not-found",
	}, " ")
	return runner.RunCommand(ctx, cmd)
}

// ReleaseExists reports whether the release is present in the namespace.
func (t ClusterTools) ReleaseExists(ctx context.Context, runner RemoteRunner, release, namespace string) bool {
	cmd := strings.Join([]string{
		t.Helm, "status", shellQuote(release),
		"--namespace", shellQuote(namespace),
	}, " ")
	_, err := runner.Capture(ctx, cmd)
	return err == nil
}

// IngressHosts returns every hostname the release's Ingresses currently serve,
// across the parent chart and any subchart (they share the release's `instance`
// label).
//
// It answers one question: what would `helm upgrade` be taking away? An Ingress
// this release owns is deleted the moment the values stop asking for it, and the
// values are rewritten from scratch on every deploy — so a host nobody restated
// is a host that goes offline. Without reading the cluster there is no way to
// know a release HAS one, since the record only says what past runs were told.
//
// Best-effort, like ServiceEndpoint: an empty result means "found none, or could
// not ask". The caller uses it to REFUSE, never to remove, so the failure mode of
// a cluster that cannot answer is the behaviour that already exists today.
func (t ClusterTools) IngressHosts(ctx context.Context, runner RemoteRunner, release, namespace string) []string {
	cmd := strings.Join([]string{
		t.Kubectl, "get", "ingress",
		"-l", shellQuote("app.kubernetes.io/instance=" + release),
		"--namespace", shellQuote(namespace),
		"-o", shellQuote("jsonpath={.items[*].spec.rules[*].host}"),
	}, " ")
	out, err := runner.Capture(ctx, cmd)
	if err != nil {
		return nil
	}
	return strings.Fields(out)
}

// ServiceEndpoint returns a URL for the deployed app, preferring an Ingress host
// and falling back to the node IP plus a NodePort.
//
// Best-effort by design: this is reported to the user at the end of a deploy,
// and a cluster whose address cannot be determined is not a failed deploy. It
// returns "" rather than an error when there is simply nothing to report.
func (t ClusterTools) ServiceEndpoint(ctx context.Context, runner RemoteRunner, release, namespace string) string {
	ns := []string{"--namespace", shellQuote(namespace)}

	// An Ingress host is the address the user actually meant.
	ingressCmd := strings.Join(append([]string{
		t.Kubectl, "get", "ingress", "-l", shellQuote("app.kubernetes.io/instance=" + release),
	}, append(ns, "-o", shellQuote("jsonpath={.items[0].spec.rules[0].host}"))...), " ")
	if host, err := runner.Capture(ctx, ingressCmd); err == nil {
		if host = strings.TrimSpace(host); host != "" {
			scheme := "http"
			tlsCmd := strings.Join(append([]string{
				t.Kubectl, "get", "ingress", "-l", shellQuote("app.kubernetes.io/instance=" + release),
			}, append(ns, "-o", shellQuote("jsonpath={.items[0].spec.tls[0].secretName}"))...), " ")
			if tls, terr := runner.Capture(ctx, tlsCmd); terr == nil && strings.TrimSpace(tls) != "" {
				scheme = "https"
			}
			return scheme + "://" + host
		}
	}

	// Otherwise a NodePort, which is reachable without any ingress controller.
	portCmd := strings.Join(append([]string{
		t.Kubectl, "get", "svc", "-l", shellQuote("app.kubernetes.io/instance=" + release),
	}, append(ns, "-o", shellQuote("jsonpath={.items[0].spec.ports[0].nodePort}"))...), " ")
	port, err := runner.Capture(ctx, portCmd)
	if err != nil || strings.TrimSpace(port) == "" {
		return ""
	}
	nodeCmd := t.Kubectl + " get nodes -o " + shellQuote("jsonpath={.items[0].status.addresses[?(@.type==\"InternalIP\")].address}")
	node, err := runner.Capture(ctx, nodeCmd)
	if err != nil || strings.TrimSpace(node) == "" {
		return ""
	}
	return "http://" + strings.Fields(node)[0] + ":" + strings.TrimSpace(port)
}

// shellQuote single-quotes an argument for the remote shell.
//
// Everything here crosses an `ssh host '<command>'` boundary, so a value with a
// space or a shell metacharacter in it — a namespace someone typed oddly, a
// path under a directory with spaces — would otherwise be re-split by the
// remote shell into arguments nobody intended.
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}
