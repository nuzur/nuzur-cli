package app

import (
	"context"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/nuzur/nuzur-cli/deploy"
	"github.com/nuzur/nuzur-cli/outputtools"
)

// stepsRunFor returns the names of the steps that would run for a given state,
// by evaluating each step's skip predicate. It runs nothing.
func stepsRunFor(st *deployState) []string {
	var names []string
	for _, s := range deploySteps() {
		if s.skip != nil && s.skip(st) {
			continue
		}
		names = append(names, s.name)
	}
	return names
}

func k8sState() *deployState {
	return &deployState{provider: deploy.ProviderK8s, s: &deploySettings{}}
}

func sshState() *deployState {
	return &deployState{provider: deploy.ProviderSSH, s: &deploySettings{}}
}

func contains(names []string, want string) bool {
	for _, n := range names {
		if n == want {
			return true
		}
	}
	return false
}

// TestK8sSkipsTheVMRuntimeSteps is the safety property of this provider.
//
// The bootstrap is the one that must never run: past installing Docker, a
// database and Caddy the cluster has no use for, it ends with `ufw --force
// enable`, whose default-deny policy severs the node's API server (16443),
// kubelet (10250), etcd and the entire NodePort range — and the teardown never
// turns it back off, so the damage outlives the deploy.
func TestK8sSkipsTheVMRuntimeSteps(t *testing.T) {
	names := stepsRunFor(k8sState())
	for _, forbidden := range []string{
		"bootstrap",
		"copy source",
		"wait for agent",
		"publish catalog",
		"read back front door",
		"issue provisioning token",
		"check cli release",
	} {
		if contains(names, forbidden) {
			t.Errorf("step %q must not run for the k8s provider; steps were %v", forbidden, names)
		}
	}
}

// TestK8sRunsItsOwnSteps: the counterpart — the k8s path must actually do its
// own work rather than silently skipping everything.
func TestK8sRunsItsOwnSteps(t *testing.T) {
	// With a connection, so the schema push has a target — see
	// TestK8sSkipsSchemaWithoutAConnection for the other half.
	st := k8sState()
	st.fromConnection = true
	names := stepsRunFor(st)
	for _, required := range []string{
		"resolve cluster",
		"stamp chart version",
		"commit and push",
		"wait for ci",
		"resolve image",
		"helm release",
		"read back cluster address",
		"apply schema",
	} {
		if !contains(names, required) {
			t.Errorf("step %q must run for the k8s provider; steps were %v", required, names)
		}
	}
}

// TestK8sSkipsSchemaWithoutAConnection: a team connection is the k8s path's
// only route to the database, since there is no agent to push through. Running
// the step anyway would address an agent that was never deployed and fail with
// an error about a box, on a provider that has no box.
func TestK8sSkipsSchemaWithoutAConnection(t *testing.T) {
	st := k8sState()
	if contains(stepsRunFor(st), "apply schema") {
		t.Error("without --connection the k8s path has no schema target and must skip the step")
	}

	st.fromConnection = true
	if !contains(stepsRunFor(st), "apply schema") {
		t.Error("with --connection the schema push must run")
	}

	// The VM path is unaffected: it pushes through the box's agent.
	ssh := sshState()
	if !contains(stepsRunFor(ssh), "apply schema") {
		t.Error("the ssh path must still apply the schema without a team connection")
	}
}

// TestSkipSchemaDecouplesCredentialsFromTheSchemaPush.
//
// --connection does two jobs: it is the schema push's target AND the only
// source deploy can write the host's credentials file from. Without a way to
// say "use the connection, but leave my database alone", declining the schema
// push meant giving up the config writing too — which is exactly the corner
// that forced writing the file by hand on the first real deploy.
func TestSkipSchemaDecouplesCredentialsFromTheSchemaPush(t *testing.T) {
	st := k8sState()
	st.fromConnection = true
	st.s.SkipSchema = true

	names := stepsRunFor(st)
	if contains(names, "apply schema") {
		t.Error("--skip-schema must leave the database alone")
	}
	// The connection is still there for the credentials file.
	if !contains(names, "write host config") {
		t.Error("--skip-schema must not disable writing the host credentials file")
	}

	// It works for the VM providers too — "don't touch my database" is not a
	// k8s-specific request.
	ssh := sshState()
	ssh.s.SkipSchema = true
	if contains(stepsRunFor(ssh), "apply schema") {
		t.Error("--skip-schema should apply to every provider")
	}
}

// TestNonK8sSkipsEveryK8sStep guards the other direction: adding this provider
// must not have changed what an ssh deploy does.
func TestNonK8sSkipsEveryK8sStep(t *testing.T) {
	names := stepsRunFor(sshState())
	for _, k8sOnly := range []string{
		"resolve cluster", "stamp chart version", "commit and push",
		"wait for ci", "resolve image", "helm release", "read back cluster address",
	} {
		if contains(names, k8sOnly) {
			t.Errorf("step %q is k8s-only but runs for ssh; steps were %v", k8sOnly, names)
		}
	}
	// And it still does its own.
	for _, required := range []string{"bootstrap", "wait for agent", "read back front door"} {
		if !contains(names, required) {
			t.Errorf("ssh deploy lost step %q; steps were %v", required, names)
		}
	}
}

// TestK8sRunShapingFlagsSkipTheRightSteps covers --no-commit / --no-wait /
// --release-only, which exist to break the one-command loop apart.
func TestK8sRunShapingFlagsSkipTheRightSteps(t *testing.T) {
	t.Run("--no-commit", func(t *testing.T) {
		st := k8sState()
		st.s.NoCommit = true
		names := stepsRunFor(st)
		if contains(names, "commit and push") {
			t.Error("--no-commit must skip the commit")
		}
		// Still waits: the point is to release code already pushed, not to skip CI.
		if !contains(names, "wait for ci") {
			t.Error("--no-commit should still wait for the CI build")
		}
	})

	t.Run("--no-wait", func(t *testing.T) {
		st := k8sState()
		st.s.NoWait = true
		names := stepsRunFor(st)
		if contains(names, "wait for ci") {
			t.Error("--no-wait must skip the CI wait")
		}
		if !contains(names, "commit and push") {
			t.Error("--no-wait should still commit")
		}
	})

	t.Run("--release-only", func(t *testing.T) {
		st := k8sState()
		st.s.ReleaseOnly = true
		names := stepsRunFor(st)
		for _, skipped := range []string{"generate app", "stamp chart version", "commit and push", "wait for ci"} {
			if contains(names, skipped) {
				t.Errorf("--release-only must skip %q", skipped)
			}
		}
		// It still has to resolve the cluster and run the release, or it does nothing.
		for _, required := range []string{"resolve cluster", "resolve image", "helm release"} {
			if !contains(names, required) {
				t.Errorf("--release-only must still run %q", required)
			}
		}
	})
}

// TestK8sValuesCarryNoCredentials pins the decision that the database
// credentials never pass through Helm. Helm stores release values in an
// in-cluster Secret that `helm get values` will print back, so a DSN placed
// there would be readable by anyone with access to the namespace.
func TestK8sValuesCarryNoCredentials(t *testing.T) {
	st := k8sState()
	st.imageRef = "ghcr.io/acme/app:sha-abc"
	st.dbDSN = "host=db.internal user=app password=hunter2 dbname=app"
	st.extPass = "hunter2"

	values := k8sValuesYAML(st)
	for _, secret := range []string{"hunter2", "password", "dsn", "db.internal"} {
		if strings.Contains(strings.ToLower(values), secret) {
			t.Errorf("generated values leak %q:\n%s", secret, values)
		}
	}
}

// TestK8sValuesAddressTheImageCorrectly: a digest and a tag are different
// fields in the chart, and setting both would make which one wins depend on
// template evaluation order rather than on what the user asked for.
func TestK8sValuesAddressTheImageCorrectly(t *testing.T) {
	t.Run("tag", func(t *testing.T) {
		st := k8sState()
		st.imageRef = "ghcr.io/acme/app:sha-abc123"
		values := k8sValuesYAML(st)
		if !strings.Contains(values, `repository: "ghcr.io/acme/app"`) {
			t.Errorf("repository not set:\n%s", values)
		}
		if !strings.Contains(values, `tag: "sha-abc123"`) {
			t.Errorf("tag not set:\n%s", values)
		}
		if !strings.Contains(values, `digest: ""`) {
			t.Errorf("digest must be explicitly cleared when deploying a tag:\n%s", values)
		}
	})

	t.Run("digest", func(t *testing.T) {
		st := k8sState()
		st.imageRef = "ghcr.io/acme/app@sha256:deadbeef"
		values := k8sValuesYAML(st)
		if !strings.Contains(values, `digest: "sha256:deadbeef"`) {
			t.Errorf("digest not set:\n%s", values)
		}
		if !strings.Contains(values, `tag: ""`) {
			t.Errorf("tag must be cleared when pinning a digest:\n%s", values)
		}
		// The repository must not keep the @digest suffix.
		if !strings.Contains(values, `repository: "ghcr.io/acme/app"`) {
			t.Errorf("repository should not carry the digest:\n%s", values)
		}
	})
}

// TestK8sAuthValuesUseTheSameImage is the property that makes the auth server
// safe to deploy as a subchart: it runs the SAME image as the API, so its
// values must carry the same reference. Nested under the subchart name, which
// is how Helm routes values into charts/<name> — without the nesting the API
// would move to the new image while the auth pod stayed on whatever the chart
// defaults said.
func TestK8sAuthValuesUseTheSameImage(t *testing.T) {
	st := k8sState()
	st.identifier = "sfapi"
	st.imageRef = "ghcr.io/mklfarha/sfapi@sha256:abc"
	st.s.Domain = "apiv2.dragium.com"
	st.s.AuthDomain = "auth.dragium.com"

	values := k8sValuesYAML(st)

	if !strings.Contains(values, "sfapi-auth:") {
		t.Errorf("auth values must be nested under the subchart name:\n%s", values)
	}
	// The digest appears twice: once for each chart.
	if got := strings.Count(values, `digest: "sha256:abc"`); got != 2 {
		t.Errorf("both charts must pin the same image, saw %d:\n%s", got, values)
	}
	for _, host := range []string{`host: "apiv2.dragium.com"`, `host: "auth.dragium.com"`} {
		if !strings.Contains(values, host) {
			t.Errorf("missing %s:\n%s", host, values)
		}
	}
}

// TestK8sAuthValuesOmittedWithoutAuthDomain: a project with no auth host must
// not have an ingress silently enabled for it.
func TestK8sAuthValuesOmittedWithoutAuthDomain(t *testing.T) {
	st := k8sState()
	st.identifier = "myapp"
	st.imageRef = "ghcr.io/acme/myapp:sha-1"

	values := k8sValuesYAML(st)
	if strings.Contains(values, "myapp-auth:") {
		t.Errorf("no auth values should be written without --auth-domain:\n%s", values)
	}
	if strings.Contains(values, "ingress:") {
		t.Errorf("no ingress should be enabled without a domain:\n%s", values)
	}
}

// ── the ingress a re-deploy used to delete ───────────────────────────────────

// ingressCluster answers the one question the release pre-flight asks — which
// hostnames the release's Ingresses currently serve — and panics on anything
// else. The guard runs before the chart is copied or helm is invoked, and a
// guard that touched the cluster for any other reason would be doing work a
// refusal is supposed to make free.
type ingressCluster struct {
	hosts    string
	captures []string
}

func (c *ingressCluster) Ping(ctx context.Context) error { return nil }
func (c *ingressCluster) SetSudo(bool)                   {}
func (c *ingressCluster) RunCommand(ctx context.Context, cmd string) error {
	panic("guard ran a command: " + cmd)
}
func (c *ingressCluster) RunScript(ctx context.Context, l, s string) error {
	panic("guard ran a script: " + l)
}
func (c *ingressCluster) CopyDir(ctx context.Context, from, to string) error {
	panic("guard copied to " + to)
}

func (c *ingressCluster) Capture(ctx context.Context, cmd string) (string, error) {
	c.captures = append(c.captures, cmd)
	if strings.Contains(cmd, "get ingress") {
		return c.hosts, nil
	}
	panic("unscripted Capture: " + cmd)
}

// releaseServing is a k8s deploy about to release over a cluster whose release
// already serves the given hosts.
func releaseServing(hosts string) (*deployState, *ingressCluster) {
	st := k8sState()
	st.identifier, st.releaseName, st.namespace = "sfapi", "sfapi", "sfapi"
	st.imageRef = "ghcr.io/mklfarha/sfapi:sha-abc"
	st.tools = deploy.ClusterTools{Helm: "helm", Kubectl: "kubectl"}
	cluster := &ingressCluster{hosts: hosts}
	st.runner = cluster
	return st, cluster
}

// TestK8sDeployCannotSilentlyRemoveALiveIngress is the bug this guard exists
// for, and it fails against the behaviour that shipped.
//
// Deploy rewrites the release's values from scratch every run. writeIngressValues
// writes NOTHING when it has no host, and the chart's own default is
// `ingress.enabled: false` — so a re-deploy that simply omitted a hostname made
// `helm upgrade` DELETE the Ingress. Nothing failed: the deploy printed success
// while the site stopped answering. --auth-domain was the worst of it, being
// recorded nowhere at all, so EVERY re-deploy dropped it.
func TestK8sDeployCannotSilentlyRemoveALiveIngress(t *testing.T) {
	t.Run("a forgotten hostname is refused, and says which flags", func(t *testing.T) {
		st, _ := releaseServing("apiv2.dragium.com auth.dragium.com")
		st.s.Domain = "apiv2.dragium.com" // --auth-domain forgotten

		err := guardIngressRemoval(context.Background(), st)
		if err == nil {
			t.Fatal("a deploy that would delete the live auth Ingress must not be allowed to run")
		}
		for _, want := range []string{"auth.dragium.com", "--auth-domain", "--domain", "--deployment"} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("error does not mention %q, so it does not say how to fix it:\n%v", want, err)
			}
		}
	})

	t.Run("forgetting every hostname is refused too", func(t *testing.T) {
		st, _ := releaseServing("apiv2.dragium.com")
		if err := guardIngressRemoval(context.Background(), st); err == nil {
			t.Fatal("a deploy with no hostname at all against a release that has one must be refused")
		}
	})

	t.Run("restating them releases normally", func(t *testing.T) {
		st, _ := releaseServing("apiv2.dragium.com auth.dragium.com")
		st.s.Domain, st.s.AuthDomain = "apiv2.dragium.com", "auth.dragium.com"
		if err := guardIngressRemoval(context.Background(), st); err != nil {
			t.Fatalf("a deploy that keeps every host must not be blocked: %v", err)
		}
	})

	t.Run("a rename is not a removal", func(t *testing.T) {
		// Moving a hostname is a legitimate deploy, and a guard that compared host
		// SETS rather than counts would refuse it — turning a safety net into a
		// wall. One in, one out.
		st, _ := releaseServing("apiv2.dragium.com")
		st.s.Domain = "api3.dragium.com"
		if err := guardIngressRemoval(context.Background(), st); err != nil {
			t.Fatalf("renaming the only host must still be possible: %v", err)
		}
	})

	t.Run("a first deploy has nothing to protect", func(t *testing.T) {
		st, _ := releaseServing("")
		if err := guardIngressRemoval(context.Background(), st); err != nil {
			t.Fatalf("a release with no Ingress cannot lose one: %v", err)
		}
		// Nor can adding one be refused.
		st.s.Domain = "apiv2.dragium.com"
		if err := guardIngressRemoval(context.Background(), st); err != nil {
			t.Fatalf("adding a host must never be refused: %v", err)
		}
	})

	t.Run("a cluster that cannot answer is not a refusal", func(t *testing.T) {
		// IngressHosts is best-effort: an unreachable or ingress-less cluster
		// returns nothing. Refusing on silence would block deploys over a kubectl
		// hiccup, which is a worse failure than the one being prevented.
		st, cluster := releaseServing("")
		cluster.hosts = "   \n"
		if err := guardIngressRemoval(context.Background(), st); err != nil {
			t.Fatalf("an empty answer must read as 'no ingress', not as a block: %v", err)
		}
	})
}

// TestPlannedIngressHostsMatchTheValues keeps the guard honest about what the
// values file actually does. The guard clears a release by counting hosts; if it
// counted a host k8sValuesYAML never writes, it would wave through exactly the
// removal it exists to stop.
func TestPlannedIngressHostsMatchTheValues(t *testing.T) {
	st := k8sState()
	st.identifier = "sfapi"
	st.imageRef = "ghcr.io/mklfarha/sfapi:sha-abc"
	st.s.Domain = "apiv2.dragium.com"
	st.s.AuthDomain = "auth.dragium.com"
	st.s.GRPCDomain = "grpc.dragium.com"

	values := k8sValuesYAML(st)
	planned := plannedIngressHosts(st)
	if got := strings.Count(values, "host: "); got != len(planned) {
		t.Errorf("the values enable %d ingress host(s) but the guard counts %d (%v):\n%s",
			got, len(planned), planned, values)
	}
	for _, h := range planned {
		if !strings.Contains(values, `host: "`+h+`"`) {
			t.Errorf("guard counts %q, which the values never write:\n%s", h, values)
		}
	}

	// And with nothing stated, the values write no ingress at all — which is
	// precisely why the guard has to run.
	bare := k8sState()
	bare.identifier = "sfapi"
	if len(plannedIngressHosts(bare)) != 0 || strings.Contains(k8sValuesYAML(bare), "ingress:") {
		t.Error("an unstated hostname must still write no ingress; the guard, not the values, is what protects the live one")
	}
}

// connState is a k8s state with a resolved team connection, as
// stepResolveAndConfigure would leave it.
func connState() *deployState {
	st := k8sState()
	st.identifier = "aburrides"
	st.fromConnection = true
	st.dbEngine = deploy.DBPostgres
	st.extHost = "db.example.com"
	st.extPort = "5432"
	st.extUser = "aburrides"
	st.extPass = "s3cr3t-p4ssw0rd"
	st.extName = "aburrides"
	st.extParams = "sslmode=require"
	return st
}

// TestCredentialsYAMLFullIncludesEverything: the full mode has to produce a
// file the app can actually start from.
func TestCredentialsYAMLFullIncludesEverything(t *testing.T) {
	got := k8sCredentialsYAML(connState(), true)
	for _, want := range []string{
		"name: aburrides", "host: db.example.com", "port: 5432",
		"user: aburrides", `pswd: "s3cr3t-p4ssw0rd"`,
		`params: "sslmode=require"`, `driver: "postgres"`,
	} {
		if !strings.Contains(got, want) {
			t.Errorf("credentials file missing %q:\n%s", want, got)
		}
	}
}

// TestCredentialsYAMLNoPasswordOmitsIt is the mode that exists so a user can
// have the tedious parts filled in without the CLI transmitting their password.
// If the password leaked in anyway the option would be actively misleading —
// worse than not offering it.
func TestCredentialsYAMLNoPasswordOmitsIt(t *testing.T) {
	st := connState()
	got := k8sCredentialsYAML(st, false)

	if strings.Contains(got, st.extPass) {
		t.Errorf("the no-password mode must not write the password:\n%s", got)
	}
	if !strings.Contains(got, `pswd: ""`) {
		t.Errorf("expected an empty pswd placeholder to fill in:\n%s", got)
	}
	if !strings.Contains(got, "TODO") {
		t.Errorf("the placeholder should say it needs filling in:\n%s", got)
	}
	// Everything else still has to be there, or the mode saves nobody anything.
	for _, want := range []string{"host: db.example.com", "user: aburrides", `driver: "postgres"`} {
		if !strings.Contains(got, want) {
			t.Errorf("no-password mode dropped %q:\n%s", want, got)
		}
	}
}

// TestWriteFileCmdTerminatesItsHeredoc is a regression test for a silent
// failure that reached a real deploy.
//
// writeFileCmd emits a heredoc, whose terminator must be ALONE on its line.
// Joining the write steps with " && " produced
//
//	NUZUR_EOF && printf ... && mv ...
//
// which never matches the delimiter, so bash swallowed the remaining commands
// as file content. `cat` still succeeded, the command exited 0, the `mv` never
// ran — and the CLI reported writing a credentials file that did not exist.
func TestWriteFileCmdTerminatesItsHeredoc(t *testing.T) {
	cmd := writeFileCmd("/etc/config/app/prod.yaml", "db:\n  - name: app\n")

	lines := strings.Split(cmd, "\n")
	var terminators int
	for _, line := range lines {
		if line == "NUZUR_EOF" {
			terminators++
		}
	}
	if terminators != 1 {
		t.Errorf("expected exactly one line that is ONLY the heredoc terminator, got %d:\n%s", terminators, cmd)
	}

	// The terminator must also be the last line, so anything appended by the
	// caller lands after the heredoc rather than inside it.
	if last := lines[len(lines)-1]; last != "NUZUR_EOF" {
		t.Errorf("the heredoc terminator must be the final line, got %q:\n%s", last, cmd)
	}
}

// TestCredentialsPathMatchesTheChartMount pins the two halves together. The
// chart mounts hostPath /etc/config at /root/prod-config and reads
// <mount>/<identifier>, so the file the CLI writes must land at
// /etc/config/<identifier>/prod.yaml. A mismatch is invisible until a pod dies
// with "db configuration not found".
func TestCredentialsPathMatchesTheChartMount(t *testing.T) {
	if got, want := k8sCredentialsPath("aburrides"), "/etc/config/aburrides/prod.yaml"; got != want {
		t.Errorf("k8sCredentialsPath = %q, want %q", got, want)
	}
	// prod.yaml, not base.yaml or anything else: the image sets ENV=prod and the
	// loader reads <dir>/base.yaml then <dir>/<ENV>.yaml.
	if !strings.HasSuffix(k8sCredentialsPath("x"), "/prod.yaml") {
		t.Error("the file must be named prod.yaml for ENV=prod to load it")
	}
}

// TestSkippedStepsAreNotReportedAsFailures.
//
// The first successful k8s deploy still exited 1 and closed by telling the user
// their connection "was NOT published" and their schema "was NOT applied" — both
// steps this provider skips by design, one of them because the user asked for
// --skip-schema. The outcome's zero value is failedDuringApply, so "never ran"
// and "ran and broke" were the same value, and a deploy that did exactly what
// was asked read as a half-failure.
func TestSkippedStepsAreNotReportedAsFailures(t *testing.T) {
	o := deployOutcome{
		catalogNotApplicable: true,
		schema:               schemaStateNotAttempted,
	}

	if got := o.summary(); got != "" {
		t.Errorf("a deploy that skipped both steps should close with no warning, got:\n%s", got)
	}
	if got := o.revisionMessage(); got != "" {
		t.Errorf("nothing failed, so the revision should record nothing, got: %q", got)
	}
	if o.summaryColor() == outputtools.Red {
		t.Error("a deliberately skipped schema step must not be reported in red")
	}
}

// TestRealFailuresAreStillReported is the other half — the fix must not silence
// an actual failure. A schema step that ran and broke still has to say so.
func TestRealFailuresAreStillReported(t *testing.T) {
	failed := deployOutcome{catalogNotApplicable: true, schema: schemaStateFailedDuringApply}
	if failed.summary() == "" {
		t.Error("a schema apply that failed must still warn")
	}
	if failed.summaryColor() != outputtools.Red {
		t.Error("a real schema failure must still be red")
	}

	// And an unpublished catalog still reports when publishing WAS applicable.
	unpublished := deployOutcome{schema: schemaStateApplied}
	if !strings.Contains(unpublished.summary(), "NOT published") {
		t.Errorf("a genuine publish failure must still be reported:\n%s", unpublished.summary())
	}
}

// TestK8sMarksItsSkippedStepsNotApplicable checks the state is actually set,
// not just that the renderer would honour it.
func TestK8sMarksItsSkippedStepsNotApplicable(t *testing.T) {
	for _, tc := range []struct {
		name string
		st   *deployState
	}{
		{"--skip-schema", func() *deployState {
			st := k8sState()
			st.fromConnection = true
			st.s.SkipSchema = true
			return st
		}()},
		{"no connection", k8sState()},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// Mirrors what stepK8sResolve sets before anything else runs.
			tc.st.outcome.catalogNotApplicable = true
			if tc.st.s.SkipSchema || !tc.st.fromConnection {
				tc.st.outcome.schema = schemaStateNotAttempted
			}
			if tc.st.outcome.schema != schemaStateNotAttempted {
				t.Error("the schema step is skipped here, so the outcome must say notAttempted")
			}
			if tc.st.outcome.summary() != "" {
				t.Errorf("no warning expected:\n%s", tc.st.outcome.summary())
			}
		})
	}
}

// TestExitCodeTreatsNotAttemptedAsSuccess.
//
// The report's exit code is the deploy's exit code, and it was derived from
// "schema != applied" — so a deploy that was TOLD not to touch the database
// exited 1. Every script and CI job wrapping the CLI would read that working
// deploy as a failure.
func TestExitCodeTreatsNotAttemptedAsSuccess(t *testing.T) {
	for _, tc := range []struct {
		state   schemaOutcomeState
		wantErr bool
	}{
		{schemaStateApplied, false},
		{schemaStateNotAttempted, false}, // asked not to run — not a failure
		{schemaStateFailedDuringApply, true},
		{schemaStateFailedBeforeSQL, true},
		{schemaStateBlocked, true},
	} {
		err := exitCodeForOutcome(deployOutcome{schema: tc.state})
		if (err != nil) != tc.wantErr {
			t.Errorf("schema state %v: exit error = %v, want error: %v", tc.state, err, tc.wantErr)
		}
	}
}

// TestK8sReusesItsRecordAcrossFailedRuns.
//
// pickPriorDeployment's reusability test is a VM one: an agent uuid, or a run
// that reached StepAgentPaired. The k8s path pairs no agent and creates no box,
// so a run that died before its release could never satisfy it — and every retry
// minted a NEW record for the same cluster and identifier. Five accumulated for
// one app in an afternoon, which also makes `--deployment <id>` ambiguous.
func TestK8sReusesItsRecordAcrossFailedRuns(t *testing.T) {
	const host, id = "203.0.113.10", "myapp"

	// A k8s run that died after recording the target but before releasing:
	// no agent, checkpoint below StepAgentPaired.
	died := deploy.Deployment{
		ID: "myapp-aaaa1111", Provider: deploy.ProviderK8s,
		Host: host, Identifier: id,
		LastCompletedStep: deploy.StepBoxRecorded,
		CreatedAt:         time.Now().Add(-time.Hour),
	}

	got := pickPriorDeployment([]deploy.Deployment{died}, host, id)
	if got == nil {
		t.Fatal("a k8s record must be reusable even with no agent — otherwise every failed run leaks a new one")
	}
	if got.ID != died.ID {
		t.Errorf("reused %q, want %q", got.ID, died.ID)
	}

	// The VM rule is untouched: an ssh run that died before pairing is still
	// NOT adopted, because that record may describe a half-built box.
	sshDied := died
	sshDied.ID = "myapp-bbbb2222"
	sshDied.Provider = deploy.ProviderSSH
	if pickPriorDeployment([]deploy.Deployment{sshDied}, host, id) != nil {
		t.Error("an unpaired ssh record must still not be adopted")
	}
}

// TestK8sCodegenRequirementsAreForced: helm and github_actions default to false
// and a project's saved config very likely says so, but with either off there
// is nothing to release.
func TestK8sCodegenRequirementsAreForced(t *testing.T) {
	provided := map[string]interface{}{"helm": false, "github_actions": false}
	forced := applyK8sCodegenRequirements(provided, nil)

	for field := range k8sRequiredCodegen {
		if !boolValue(provided, field) {
			t.Errorf("%s must be forced on for the k8s provider", field)
		}
	}
	sort.Strings(forced)
	if len(forced) == 0 {
		t.Error("the override should be reported so the user knows their config was changed")
	}

	// Already-true values are not reported as changes — a deploy should not
	// announce that it did something it did not do.
	quiet := applyK8sCodegenRequirements(map[string]interface{}{
		"helm": true, "github_actions": true, "dockerfile": true,
	}, nil)
	if len(quiet) != 0 {
		t.Errorf("nothing was changed, so nothing should be reported; got %v", quiet)
	}
}

// TestK8sValuesServeGRPCOnItsOwnHost covers the seam between the CLI and the
// generated chart, which is invisible at both ends.
//
// nginx.ingress.kubernetes.io/backend-protocol is an annotation on the Ingress
// OBJECT, so one Ingress speaks exactly one protocol to its backend and an app
// serving both is served from two objects on two hostnames. The chart therefore
// exposes two keys — `ingress` and `grpcIngress` — and helm SILENTLY IGNORES a
// values key no template reads. Write the gRPC host under the wrong key, or not
// at all, and the deploy still succeeds while reporting an address nothing
// answers on.
func TestK8sValuesServeGRPCOnItsOwnHost(t *testing.T) {
	st := k8sState()
	st.imageRef = "ghcr.io/acme/app:sha-abc"
	st.s.Domain = "api.example.com"
	st.s.GRPCDomain = "grpc.example.com"

	values := k8sValuesYAML(st)

	// Two separate blocks, each enabled, each with its own host.
	for _, want := range []string{
		"ingress:\n  enabled: true",
		"grpcIngress:\n  enabled: true",
		`host: "api.example.com"`,
		`host: "grpc.example.com"`,
	} {
		if !strings.Contains(values, want) {
			t.Errorf("values missing %q:\n%s", want, values)
		}
	}

	// The gRPC host must not land in the HTTP block: it would be served over
	// HTTP/1.1 to an HTTP/2 backend, which fails at request time, not at deploy.
	httpBlock := values[strings.Index(values, "ingress:"):strings.Index(values, "grpcIngress:")]
	if strings.Contains(httpBlock, "grpc.example.com") {
		t.Errorf("the gRPC host leaked into the HTTP ingress block:\n%s", httpBlock)
	}

	// A project that serves no gRPC states no gRPC host, and then nothing is
	// written — the chart's own default (disabled) is what applies.
	bare := k8sState()
	bare.imageRef = st.imageRef
	bare.s.Domain = "api.example.com"
	if strings.Contains(k8sValuesYAML(bare), "grpcIngress:") {
		t.Errorf("no --grpc-domain must write no grpcIngress block:\n%s", k8sValuesYAML(bare))
	}
}
