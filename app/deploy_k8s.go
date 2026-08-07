// Kubernetes deploy steps.
//
// The VM path installs a runtime on a box: Docker, a database, a systemd unit,
// Caddy, an agent, ufw. This path installs nothing. The generated repo already
// carries a Helm chart and a CI workflow, so a deploy is: stamp the chart
// version, commit, let CI build the image, then run `helm upgrade --install` on
// a machine that can reach the cluster.
//
// Everything reaches the cluster over SSH, through the same RemoteRunner the VM
// path uses — helm and kubectl run ON the host (microk8s ships both), so no
// kubeconfig is needed locally and the API server never has to be exposed.
package app

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/manifoldco/promptui"
	"github.com/urfave/cli"

	"github.com/nuzur/nuzur-cli/deploy"
	"github.com/nuzur/nuzur-cli/outputtools"
)

// k8sCIWaitTimeout bounds the wait for a CI image build. Generous: a cold Go
// build with no layer cache is minutes, and timing out on a build that would
// have succeeded costs a deploy for no reason. --no-wait skips it entirely.
const k8sCIWaitTimeout = 20 * time.Minute

// k8sHelmWaitTimeout bounds `helm upgrade --wait`. It covers image pull plus
// startup; past that, something is wrong and --atomic rolls the release back.
const k8sHelmWaitSecs = 300

// isK8s is the skip predicate for every step below, and its inverse guards the
// VM-only steps in deploySteps.
func isK8s(st *deployState) bool { return st.provider == deploy.ProviderK8s }

func notK8s(st *deployState) bool { return !isK8s(st) }

// ── naming ───────────────────────────────────────────────────────────────────

// stepK8sResolve settles everything the release is addressed by, and proves the
// cluster is reachable — before anything is generated, committed or built.
//
// Reachability is checked here, at the cheapest possible moment, for the same
// reason the pipeline puts every free check before anything that costs money: a
// cluster the host cannot talk to should not cost a codegen run, a commit and a
// CI build before saying so.
func (i *Implementation) stepK8sResolve(ctx context.Context, st *deployState) error {
	st.namespace = firstNonEmpty(strings.TrimSpace(st.s.Namespace), st.identifier)
	st.releaseName = firstNonEmpty(strings.TrimSpace(st.s.Release), st.identifier)

	// Tell the closing report what this provider does NOT do, so it stops
	// reporting those as failures.
	//
	// There is no agent on this path, so there is no connection catalog to
	// publish — "not published" would be true and useless. And when the schema
	// step is skipped, the outcome's zero value would otherwise read as "failed
	// during apply" and send the user to audit a database this deploy never
	// opened a connection to.
	st.outcome.catalogNotApplicable = true
	if st.s.SkipSchema || !st.fromConnection {
		st.outcome.schema = schemaStateNotAttempted
	}

	// Normally set by `generate app`, which runs earlier. --release-only skips
	// that step, so resolve it the same way here — otherwise the chart path
	// would be built from an empty workspace and point at ./.helm/<identifier>,
	// which is either missing or, worse, some other project's chart.
	if st.workspaceDir == "" {
		ws, err := resolveWorkspace(st.s.SourceDir, st.prior, st.identifier)
		if err != nil {
			return err
		}
		st.workspaceDir = ws
	}

	// The chart lives under the APP directory, not the workspace root: the
	// generator writes into <workspace>/<identifier>/, so the chart is at
	// <workspace>/<identifier>/.helm/<identifier>. findSourceRoot is what the
	// rest of the pipeline already uses to locate that directory (it finds the
	// Dockerfile), rather than re-deriving it from the identifier — the
	// generated folder is named from the GENERATOR's identifier, which
	// --identifier can move independently of the deployment's.
	appDir := st.sourceRoot
	if appDir == "" {
		// `generate app` normally sets this; --release-only skips that step.
		var err error
		if appDir, err = findSourceRoot(st.workspaceDir); err != nil {
			return fmt.Errorf("locating the generated app under %s: %w", st.workspaceDir, err)
		}
	}
	// Everything that follows works from the APP directory, not the workspace
	// root. The generator writes into <workspace>/<identifier>/, so the workspace
	// root is the repo's PARENT — asking git about it finds no repository at all.
	st.appDir = appDir
	st.chartDir = filepath.Join(appDir, ".helm", st.identifier)
	if _, err := os.Stat(filepath.Join(st.chartDir, "Chart.yaml")); err != nil {
		return fmt.Errorf(
			"no Helm chart at %s — the generator emits one when `helm` is enabled, which this provider turns on automatically. If you passed --release-only, run a full deploy first: %w",
			st.chartDir, err)
	}

	tools, err := deploy.DetectClusterTools(ctx, st.runner, st.s.HelmCmd, st.s.KubectlCmd)
	if err != nil {
		return err
	}
	st.tools = tools
	if err := tools.ReachCluster(ctx, st.runner); err != nil {
		return err
	}

	flavour := "kubernetes"
	if tools.IsMicroK8s() {
		flavour = "microk8s"
	}
	outputtools.PrintlnColoredErr(
		fmt.Sprintf("Cluster ready (%s) — release %q in namespace %q", flavour, st.releaseName, st.namespace),
		outputtools.Blue)

	// The two things the cluster must already have, checked while it is still
	// free to say so. Both are the operator's to create, and both fail LATER as
	// a pod that never becomes Ready — CrashLoopBackOff for a missing config
	// file, ImagePullBackOff for a missing pull secret — which is a slow and
	// unobvious way to learn about a missing file.
	i.warnMissingK8sPrerequisites(ctx, st)
	return nil
}

// warnMissingK8sPrerequisites checks the operator-owned setup and warns, rather
// than failing.
//
// A warning and not an error because both checks can be wrong: the credentials
// file only has to exist on the nodes that will schedule the pod (which may not
// be the host we SSH to), and a public image needs no pull secret. Refusing to
// deploy on a guess would be worse than a deploy that explains itself.
func (i *Implementation) warnMissingK8sPrerequisites(ctx context.Context, st *deployState) {
	credsPath := fmt.Sprintf("/etc/config/%s/prod.yaml", st.identifier)
	if _, err := st.runner.Capture(ctx, "test -f "+credsPath+" && echo found"); err != nil {
		outputtools.PrintlnColoredErr(fmt.Sprintf(
			"warning: %s not found on %s.\n"+
				"  The app reads its database credentials from that file — without it the pod starts and exits\n"+
				"  with \"db configuration not found\". Create it (0600) with your db block:\n"+
				"      sudo mkdir -p /etc/config/%s\n"+
				"      sudo nano %s\n"+
				"  See docs/k8s-deploy.md. On a multi-node cluster it must exist on every schedulable node,\n"+
				"  so this check can be a false alarm.",
			credsPath, st.s.Host, st.identifier, credsPath), outputtools.Yellow)
	}

	secretCmd := fmt.Sprintf("%s -n %s get secret ghcr-login-secret", st.tools.Kubectl, shellQuoteArg(st.namespace))
	if _, err := st.runner.Capture(ctx, secretCmd); err != nil {
		outputtools.PrintlnColoredErr(fmt.Sprintf(
			"warning: no ghcr-login-secret in namespace %q.\n"+
				"  ghcr.io packages are private by default, so the pod would fail with ImagePullBackOff. Create it:\n"+
				"      kubectl -n %s create secret docker-registry ghcr-login-secret \\\n"+
				"        --docker-server=ghcr.io --docker-username=<user> --docker-password=<token>\n"+
				"  Ignore this if your package is public.",
			st.namespace, st.namespace), outputtools.Yellow)
	}
}

// shellQuoteArg single-quotes a value for the remote shell. The deploy package
// has its own copy for the commands it builds; this is for the two checks above.
func shellQuoteArg(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// ── the credentials file ─────────────────────────────────────────────────────

// How much of the credentials file deploy is allowed to write.
//
// This is a privacy decision, not a convenience one. Everywhere else on the k8s
// path the database password stays between the operator and the node: the CLI
// never sees it and it never enters Helm. Writing the file means the CLI holds
// the password in memory and sends it over SSH — which is exactly what the VM
// path has always done, but it is not what someone who chose this path may
// expect, so it is asked rather than assumed.
const (
	writeConfigAsk        = ""            // ask, or skip when non-interactive
	writeConfigFull       = "full"        // everything, password included
	writeConfigNoPassword = "no-password" // everything else; password left blank
	writeConfigSkip       = "skip"        // write nothing
)

// stepK8sWriteConfig offers to write /etc/config/<identifier>/prod.yaml on the
// host from the resolved team connection.
//
// Only when a connection was given (--connection is what resolves credentials
// at all) and only when the file is ABSENT: an existing file is the operator's,
// and silently overwriting it would discard hand-tuned settings the generated
// one knows nothing about.
func (i *Implementation) stepK8sWriteConfig(ctx context.Context, st *deployState) error {
	path := k8sCredentialsPath(st.identifier)

	if _, err := st.runner.Capture(ctx, "test -f "+path+" && echo found"); err == nil {
		outputtools.PrintlnColoredErr("Using the existing "+path+" on the host.", outputtools.Blue)
		return nil
	}
	if !st.fromConnection {
		// Nothing to write from. The pre-flight warning already said the file is
		// missing and how to create it.
		return nil
	}

	mode := strings.TrimSpace(st.s.WriteConfig)
	if mode == writeConfigAsk {
		var err error
		if mode, err = i.promptWriteConfig(path); err != nil {
			return err
		}
	}
	if mode == writeConfigSkip {
		outputtools.PrintlnColoredErr(
			"Not writing "+path+" — create it on the host before the pods can start (see docs/k8s-deploy.md).",
			outputtools.Yellow)
		return nil
	}

	withPassword := mode == writeConfigFull
	content := k8sCredentialsYAML(st, withPassword)

	// Written via a temp file and mv so a broken connection cannot leave a
	// half-written config that the app would read and fail on obscurely.
	//
	// The steps are joined by NEWLINES, with `set -e` for the fail-fast that
	// `&&` would otherwise give. That is not a style choice: writeFileCmd emits
	// a heredoc, whose terminator must be alone on its line. Chaining with
	// ` && ` produced `NUZUR_EOF && printf ... && mv ...`, which never matches
	// the delimiter — so bash swallowed the remaining commands as file content,
	// the mv never ran, and the whole thing still exited 0. The CLI reported
	// writing a file that did not exist.
	tmp := path + ".tmp"
	steps := []string{
		"set -e",
		"mkdir -p " + filepath.Dir(path),
		writeFileCmd(tmp, content),
	}

	// A JWT project needs a signing key, and the connection has nothing to say
	// about one. Generated ON THE HOST rather than here, following what the VM
	// bootstrap has always done: the key is a secret this CLI has no reason to
	// ever hold, and it is written once — the file is never rewritten, so the
	// key survives every later deploy. Rotating it invalidates issued tokens,
	// which is not something a re-deploy should do silently.
	if st.jwtAuth {
		steps = append(steps,
			`printf '\nauth:\n  jwt:\n    key: %s\n' "$(openssl rand -hex 32)" >> `+tmp)
	}

	steps = append(steps, "chmod 600 "+tmp, "mv "+tmp+" "+path)
	if err := st.runner.RunCommand(ctx, strings.Join(steps, "\n")); err != nil {
		return fmt.Errorf("writing %s: %w", path, err)
	}

	// Confirm the file is actually there before saying so. The heredoc bug above
	// exited 0 while writing nothing, and a deploy that claims to have written
	// credentials it did not write sends you looking anywhere but here.
	if _, err := st.runner.Capture(ctx, "test -f "+path+" && echo found"); err != nil {
		return fmt.Errorf("wrote %s but it is not there afterwards — the remote write silently did nothing", path)
	}

	jwtNote := ""
	if st.jwtAuth {
		jwtNote = " A JWT signing key was generated on the host."
	}
	if withPassword {
		outputtools.PrintlnColoredErr("Wrote "+path+" (0600) from the team connection."+jwtNote, outputtools.Blue)
	} else {
		outputtools.PrintlnColoredErr(
			"Wrote "+path+" (0600) WITHOUT the password.\n"+
				"  Fill in `pswd:` on the host before the pods can start:\n"+
				"      sudo nano "+path,
			outputtools.Yellow)
	}
	return nil
}

// isInteractive reports whether there is a person at the terminal to answer.
//
// stdin being a character device is the signal: a piped or redirected stdin
// means a script, CI, or an agent, and a prompt there would either hang or be
// answered by whatever bytes happened to be next.
func isInteractive() bool {
	fi, err := os.Stdin.Stat()
	if err != nil {
		return false
	}
	return fi.Mode()&os.ModeCharDevice != 0
}

// promptWriteConfig asks how much to write. Non-interactive runs write nothing:
// transmitting a password is not something to do because nobody was there to
// say no.
func (i *Implementation) promptWriteConfig(path string) (string, error) {
	if !isInteractive() {
		outputtools.PrintlnColoredErr(
			"Not writing "+path+" (non-interactive). Use --write-config full|no-password to choose explicitly.",
			outputtools.Yellow)
		return writeConfigSkip, nil
	}

	outputtools.PrintlnColoredErr(
		"\n"+path+" does not exist on the host. The app cannot start without it.\n"+
			"Deploy can write it from the team connection you passed — which means this CLI\n"+
			"reads the database password and sends it to the host over SSH.",
		outputtools.Yellow)

	items := []string{
		"Write it for me, including the password",
		"Write it without the password (I'll fill in `pswd:` myself)",
		"Don't write it — I'll create the file myself",
	}
	p := promptui.Select{
		Label: "Create " + path + "?",
		Items: items,
		Templates: &promptui.SelectTemplates{
			Label:    "{{ . }}",
			Active:   "↠ {{ . | cyan }}",
			Inactive: "  {{ . | cyan }}",
			Selected: "↠ {{ . | red }}",
		},
	}
	idx, _, err := p.Run()
	if err != nil {
		return "", fmt.Errorf("choosing how to create %s: %w", path, err)
	}
	switch idx {
	case 0:
		return writeConfigFull, nil
	case 1:
		return writeConfigNoPassword, nil
	default:
		return writeConfigSkip, nil
	}
}

// k8sCredentialsPath is where the chart's hostPath mount expects this app's
// operator-managed config. Kept next to the chart's own defaults
// (HelmConfig.CredentialsHostPath / ConfigDirName) — if those move, this must.
func k8sCredentialsPath(identifier string) string {
	return "/etc/config/" + identifier + "/prod.yaml"
}

// k8sCredentialsYAML renders prod.yaml from the resolved connection.
//
// Only the keys that must differ from the image's own base.yaml: CONFIG layers
// the two, later winning, so anything omitted here keeps the generated value.
func k8sCredentialsYAML(st *deployState, withPassword bool) string {
	var b strings.Builder
	b.WriteString("# Written by nuzur deploy from the team connection.\n")
	b.WriteString("# Owned by you: deploy never overwrites an existing file.\n")
	b.WriteString("db:\n")
	fmt.Fprintf(&b, "  - name: %s\n", st.extName)
	fmt.Fprintf(&b, "    host: %s\n", st.extHost)
	fmt.Fprintf(&b, "    port: %s\n", st.extPort)
	fmt.Fprintf(&b, "    user: %s\n", st.extUser)
	if withPassword {
		fmt.Fprintf(&b, "    pswd: %q\n", st.extPass)
	} else {
		b.WriteString("    pswd: \"\"   # TODO: fill this in\n")
	}
	fmt.Fprintf(&b, "    params: %q\n", st.extParams)
	fmt.Fprintf(&b, "    driver: %q\n", string(st.dbEngine))
	return b.String()
}

// ── chart version ────────────────────────────────────────────────────────────

// stepK8sStampChart bumps the chart version and writes it into Chart.yaml.
//
// The generator emits a fixed version and wipes .helm/ on every run, so the
// version cannot live in the file — it is derived state, recomputed here each
// deploy from what the last one recorded.
//
// It has to move even when nothing else did: the version is stamped into the
// pod template labels, so it is what actually rolls the pods when the image
// reference has not changed (a moving tag resolves to the same string forever).
func (i *Implementation) stepK8sStampChart(ctx context.Context, st *deployState) error {
	chartYAML := filepath.Join(st.chartDir, "Chart.yaml")

	var prior string
	if st.prior != nil {
		prior = st.prior.ChartVersion
	}
	if prior == "" {
		// No record, but the repo may already carry a released chart — a first
		// deploy of an existing project, or one whose record was removed.
		// Continuing from the file beats restarting the version history and
		// overwriting a published chart of the same name.
		if v, err := deploy.ReadChartVersion(chartYAML); err == nil && v != "0.0.1" {
			prior = v
		}
	}

	st.chartVersion = deploy.NextChartVersion(prior)
	if err := deploy.StampChartVersion(chartYAML, st.chartVersion); err != nil {
		return err
	}
	outputtools.PrintlnColoredErr("Chart version "+st.chartVersion, outputtools.Blue)
	return nil
}

// ── commit + CI ──────────────────────────────────────────────────────────────

// stepK8sCommitPush commits the generated workspace and pushes it, which is
// what triggers the image build.
//
// Staging is scoped to the workspace path (see deploy.GitRepo): in a repo where
// the generated app is one directory among many, a deploy must not sweep up
// whatever else the user had in progress.
func (i *Implementation) stepK8sCommitPush(ctx context.Context, st *deployState) error {
	repo, err := deploy.DiscoverGitRepo(ctx, st.appDir)
	if err != nil {
		return err
	}
	st.gitRoot = repo.Root

	changed, err := repo.HasChanges(ctx)
	if err != nil {
		return err
	}
	if !changed {
		// Nothing to commit is normal — a re-deploy of unchanged code. The
		// existing HEAD is what CI built, so carry on with it.
		sha, err := repo.HeadSHA(ctx)
		if err != nil {
			return err
		}
		st.commitSHA = sha
		outputtools.PrintlnColoredErr("No changes to commit; deploying "+shortSHA(sha), outputtools.Blue)
		return nil
	}

	msg := fmt.Sprintf("nuzur deploy: %s chart %s", st.identifier, st.chartVersion)
	outputtools.PrintlnColoredErr("Committing and pushing the generated app...", outputtools.Blue)
	sha, err := repo.CommitAndPush(ctx, msg)
	if err != nil {
		return err
	}
	st.commitSHA = sha
	outputtools.PrintlnColoredErr("Pushed "+shortSHA(sha)+" to "+repo.Branch, outputtools.Blue)
	return nil
}

// stepK8sWaitCI waits for the image build for the pushed commit.
func (i *Implementation) stepK8sWaitCI(ctx context.Context, st *deployState) error {
	if st.commitSHA == "" {
		// --no-commit: read HEAD so there is still something to wait on.
		repo, err := deploy.DiscoverGitRepo(ctx, st.appDir)
		if err != nil {
			return err
		}
		st.gitRoot = repo.Root
		if st.commitSHA, err = repo.HeadSHA(ctx); err != nil {
			return err
		}
		// A commit GitHub has never seen can have no workflow run, and waiting
		// for one would block until the timeout with nothing to report.
		if !repo.PushedSHAExistsOnRemote(ctx, st.commitSHA) {
			return fmt.Errorf(
				"HEAD (%s) has not been pushed, so no CI build exists for it — push it, drop --no-commit, or use --no-wait to release an already-published image",
				shortSHA(st.commitSHA))
		}
	}

	outputtools.PrintlnColoredErr("Waiting for the CI image build...", outputtools.Blue)
	return deploy.WaitForImageBuild(ctx, deploy.CIWaitOptions{
		RepoRoot: st.gitRoot,
		SHA:      st.commitSHA,
		Workflow: deploy.ImageWorkflowFile(st.identifier),
		Timeout:  k8sCIWaitTimeout,
		OnProgress: func(state string) {
			outputtools.PrintlnColoredErr("  "+state, outputtools.Gray)
		},
	})
}

// ── image ────────────────────────────────────────────────────────────────────

// stepK8sResolveImage settles the exact image the release will run.
//
// This is the step that replaces pasting a digest into values.yaml by hand.
func (i *Implementation) stepK8sResolveImage(ctx context.Context, st *deployState) error {
	// --release-only repeats the last deploy: no new code, no new build, so the
	// image and chart version are whatever that deploy recorded. Without this it
	// would fall through to deriving a tag from a commit it never made.
	if st.s.ReleaseOnly {
		if st.prior == nil || st.prior.ImageRef == "" {
			return fmt.Errorf("--release-only needs a previous deploy to repeat, and no recorded image was found for %s", st.identifier)
		}
		st.imageRef = st.prior.ImageRef
		st.chartVersion = st.prior.ChartVersion
		outputtools.PrintlnColoredErr(
			fmt.Sprintf("Re-releasing chart %s with image %s", st.chartVersion, st.imageRef), outputtools.Blue)
		return nil
	}

	st.imageRepo = strings.TrimSpace(st.s.ImageRepo)
	if st.imageRepo == "" {
		return fmt.Errorf(
			"cannot tell which image to deploy — pass --image-repo (e.g. ghcr.io/<owner>/<repo>/%s), or set image_repo in your deploy config",
			st.identifier)
	}

	tag := strings.TrimSpace(st.s.ImageTag)
	if tag == "" {
		if st.commitSHA == "" {
			return fmt.Errorf("no commit to derive an image tag from — pass --image-tag")
		}
		tag = deploy.ImageTagForSHA(st.commitSHA)
	}

	if st.s.PinDigest {
		digest, err := deploy.ResolveImageDigest(ctx, st.gitRoot, st.imageRepo, tag)
		if err != nil {
			return err
		}
		st.imageRef = st.imageRepo + "@" + digest
	} else {
		st.imageRef = st.imageRepo + ":" + tag
	}

	outputtools.PrintlnColoredErr("Image "+st.imageRef, outputtools.Blue)
	return nil
}

// ── release ──────────────────────────────────────────────────────────────────

// stepK8sRelease copies the chart to the host and runs helm.
//
// The chart goes to the host rather than being installed from a registry so the
// release always matches the code this deploy just generated — an OCI chart
// published by CI lags the commit, and installing that would silently deploy the
// previous chart with this run's image.
func (i *Implementation) stepK8sRelease(ctx context.Context, st *deployState) error {
	// Before anything is copied or applied: would this release take a live front
	// door down? See guardIngressRemoval. First, so a refusal costs nothing.
	if err := guardIngressRemoval(ctx, st); err != nil {
		return err
	}

	remoteChart := deploy.RemoteChartDir(st.releaseName)
	if err := st.runner.RunCommand(ctx, "rm -rf "+remoteChart); err != nil {
		return err
	}
	outputtools.PrintlnColoredErr("Copying the chart to the host...", outputtools.Blue)
	if err := st.runner.CopyDir(ctx, st.chartDir, remoteChart); err != nil {
		return err
	}

	// Values deploy owns. Deliberately free of secrets: the database
	// credentials come from the operator-managed file on the node, so nothing
	// sensitive ends up in Helm's stored release values, which anyone with
	// `helm get values` can read.
	values := k8sValuesYAML(st)
	remoteValues := deploy.RemoteValuesPath(st.releaseName)
	if err := st.runner.RunCommand(ctx, writeFileCmd(remoteValues, values)); err != nil {
		return err
	}

	// Layer order, lowest first:
	//
	//	<chart>/values.yaml        generated defaults (implicit, helm reads it)
	//	<chart>/values-custom.yaml the USER's, never overwritten by regeneration
	//	remoteValues               this deploy's image ref and resolved hostnames
	//	--chart-values             an explicit per-run override
	//
	// The overlay sits BELOW deploy's values deliberately. It is the one chart
	// file regeneration preserves, so it is also the one most likely to go stale:
	// an image tag or hostname left in it months ago would otherwise silently
	// override what this release resolved, and the deploy would report an image
	// the cluster is not running. Everything deploy does NOT write — replicas,
	// autoscaling, resources, TLS, annotations — is the user's, and helm merges
	// maps key by key, so a tls block there combines with the hosts written here.
	var valueFiles []string
	remoteOverlay := remoteChart + "/values-custom.yaml"
	if hasRemoteFile(ctx, st, remoteOverlay) {
		valueFiles = append(valueFiles, remoteOverlay)
		outputtools.PrintlnColoredErr(
			"Applying values-custom.yaml from the chart (your overlay; deploy's image and hostnames still win).",
			outputtools.Blue)
	}
	valueFiles = append(valueFiles, remoteValues)

	if extra := strings.TrimSpace(st.s.ChartValues); extra != "" {
		// Applied last so an operator can override anything deploy decided.
		//
		// Shipped as CONTENT, not copied: CopyDir tars a directory
		// (`tar czf - -C <dir> .`), so pointing it at the single file this flag
		// documents failed every time — tar cannot chdir into a regular file, and
		// the remote side made a DIRECTORY where helm expected a values file.
		body, err := os.ReadFile(extra)
		if err != nil {
			return fmt.Errorf("reading --chart-values %s: %w", extra, err)
		}
		remoteExtra := remoteChart + "-user-values.yaml"
		if err := st.runner.RunCommand(ctx, writeFileCmd(remoteExtra, string(body))); err != nil {
			return fmt.Errorf("copying --chart-values %s: %w", extra, err)
		}
		valueFiles = append(valueFiles, remoteExtra)
	}

	// Vendor any subcharts before installing. helm refuses to install a chart
	// whose declared dependencies are not in charts/, and the chart is already
	// on the host by this point — sfapi declares sfauthserver, so this is not a
	// hypothetical shape.
	if err := st.tools.UpdateDependencies(ctx, st.runner, remoteChart); err != nil {
		return fmt.Errorf("resolving chart dependencies: %w", err)
	}

	outputtools.PrintlnColoredErr("Releasing with helm...", outputtools.Blue)
	if err := st.tools.UpgradeRelease(ctx, st.runner, deploy.ReleaseOptions{
		Release:     st.releaseName,
		Namespace:   st.namespace,
		ChartDir:    remoteChart,
		ValuesFiles: valueFiles,
		Wait:        true,
		TimeoutSecs: k8sHelmWaitSecs,
	}); err != nil {
		return err
	}

	// Record the release WITH its checkpoint, in one write.
	//
	// Not deferred to `finalize record`: an interrupt between here and there
	// would leave a running release the record has never heard of, so the next
	// run would re-mint the chart version this one just published, and
	// --release-only would have no image to repeat.
	dep, err := deploy.MutateDeployment(st.depID, func(d *deploy.Deployment) {
		d.Namespace = st.namespace
		d.ReleaseName = st.releaseName
		d.ChartVersion = st.chartVersion
		d.ImageRef = st.imageRef
		d.LastCompletedStep = deploy.StepReleased
	})
	if err != nil {
		return err
	}
	st.dep = dep
	return nil
}

// stepK8sReadbackURL reports where the app is reachable.
//
// Best-effort: it replaces the VM path's read of a file the bootstrap wrote,
// and a cluster whose address cannot be determined is not a failed deploy.
func (i *Implementation) stepK8sReadbackURL(ctx context.Context, st *deployState) error {
	if url := st.tools.ServiceEndpoint(ctx, st.runner, st.releaseName, st.namespace); url != "" {
		st.publicURL = url
		st.useHTTPS = strings.HasPrefix(url, "https://")
	}
	return nil
}

// ── destroy ──────────────────────────────────────────────────────────────────

// uninstallK8sRelease removes the Helm release recorded for a deployment.
//
// The namespace stays. It may hold other releases, other projects, or things
// the user put there by hand, and none of that is destroy's to remove — the
// deploy created the namespace only as a side effect of --create-namespace.
func (i *Implementation) uninstallK8sRelease(ctx context.Context, c *cli.Context, dep *deploy.Deployment) error {
	release := firstNonEmpty(dep.ReleaseName, dep.Identifier)
	namespace := firstNonEmpty(dep.Namespace, dep.Identifier)

	port := c.Int("port")
	if port == 0 {
		port = dep.Port
	}
	target := deploy.Target{
		Host:    dep.Host,
		User:    firstNonEmpty(c.String("user"), dep.User),
		Port:    port,
		KeyPath: c.String("ssh-key"),
	}
	runner := i.sshRunner(target)
	runner.SetSudo(c.Bool("sudo") || target.User != "root")

	tools, err := deploy.DetectClusterTools(ctx, runner, c.String("helm-cmd"), c.String("kubectl-cmd"))
	if err != nil {
		return err
	}

	outputtools.PrintlnColoredErr(
		fmt.Sprintf("Uninstalling the Helm release %q from namespace %q (the namespace is left in place)...", release, namespace),
		outputtools.Blue)
	return tools.UninstallRelease(ctx, runner, release, namespace)
}

// ── helpers ──────────────────────────────────────────────────────────────────

// k8sValuesYAML renders the values deploy controls.
//
// Only the image and a digest/tag choice: everything else about the chart —
// ports, probes, the credentials mount — is the generator's, and overriding it
// from here would mean two places decide the same thing.
func k8sValuesYAML(st *deployState) string {
	var b strings.Builder
	b.WriteString("# Generated by nuzur deploy. Do not edit; it is rewritten every release.\n")
	writeImageValues(&b, "", st.imageRef)
	writeIngressValues(&b, "", strings.TrimSpace(st.s.Domain), "ingress")

	// The gRPC side is a SECOND Ingress object, not another rule on the first:
	// nginx.ingress.kubernetes.io/backend-protocol is an annotation on the Ingress
	// OBJECT, so one Ingress speaks exactly one protocol to its backend. An app
	// serving both is therefore served on two hostnames, from two objects, out of
	// the one chart and the one release.
	//
	// The annotations that make h2c work are baked into the chart's template
	// rather than written here, so no values file can drop them.
	writeIngressValues(&b, "", strings.TrimSpace(st.s.GRPCDomain), "grpcIngress")

	// The auth server runs the SAME image, so its values are written from the
	// same imageRef. Nesting under the subchart name is how Helm routes values
	// into charts/<name>; leaving it out would deploy the API's new image beside
	// an auth pod still on whatever the chart defaults said.
	if auth := strings.TrimSpace(st.s.AuthDomain); auth != "" {
		fmt.Fprintf(&b, "%s:\n", st.identifier+"-auth")
		b.WriteString("  enabled: true\n")
		writeImageValues(&b, "  ", st.imageRef)
		writeIngressValues(&b, "  ", auth, "ingress")
	}

	return b.String()
}

// ── keeping a live Ingress alive ─────────────────────────────────────────────

// plannedIngressHosts lists the hosts this run's values will enable an Ingress
// for, in the order k8sValuesYAML writes them.
//
// It sits beside k8sValuesYAML because the two must agree: this is the promise
// the guard below checks, and a host that stops being written there has to stop
// being counted here or the guard would clear a release that removes it.
func plannedIngressHosts(st *deployState) []string {
	var hosts []string
	if d := strings.TrimSpace(st.s.Domain); d != "" {
		hosts = append(hosts, d)
	}
	if a := strings.TrimSpace(st.s.AuthDomain); a != "" {
		hosts = append(hosts, a)
	}
	if g := strings.TrimSpace(st.s.GRPCDomain); g != "" {
		hosts = append(hosts, g)
	}
	return hosts
}

// guardIngressRemoval refuses a release that would leave the app serving fewer
// hostnames than it does right now.
//
// The bug it exists for: deploy rewrites the values file from scratch every run,
// writeIngressValues writes NOTHING when it has no host, and the chart's own
// default is `ingress.enabled: false` — so `helm upgrade` deletes the Ingress and
// the site goes offline. Nothing in that sequence is an error; a deploy that
// forgot --auth-domain reports complete success.
//
// The record now carries every hostname and a re-deploy adopts them
// (applyDeploymentSelector), which is the real fix for anything deployed from
// here on. This is the floor under it, and it holds where the record cannot:
// releases recorded before those fields existed, runs that select their target
// by --host rather than --deployment, and a --release-only that repeats an image
// but not a flag.
//
// The invariant is stated in counts, not names: a deploy may ADD a hostname and
// may RENAME one (api.example.com → api2.example.com is a legitimate move, and
// comparing host sets would refuse it), but it may not serve fewer than the
// release already does. Each hostname flag contributes exactly one Ingress, so
// one forgotten flag is always one Ingress fewer.
//
// A user who really wants an Ingress gone deletes it, and then there is nothing
// to protect. That is the escape hatch, and the error says so.
func guardIngressRemoval(ctx context.Context, st *deployState) error {
	live := st.tools.IngressHosts(ctx, st.runner, st.releaseName, st.namespace)
	return ingressRemovalError(st.releaseName, st.namespace, live, plannedIngressHosts(st))
}

// ingressRemovalError is guardIngressRemoval's decision, split out from the
// cluster read so it can be exercised without one.
func ingressRemovalError(release, namespace string, live, planned []string) error {
	if len(live) <= len(planned) {
		return nil
	}
	keeping := "none at all"
	if len(planned) > 0 {
		keeping = strings.Join(planned, ", ")
	}
	return fmt.Errorf(
		"refusing to release: %q in namespace %q currently serves %d Ingress host(s) — %s — and this deploy resolved only %d (%s), so `helm upgrade` would DELETE the rest and take those addresses offline.\n"+
			"Deploy rewrites the release's values every run, and a hostname it was not given is a hostname the chart disables (its default is ingress.enabled: false). Nothing about that failure is visible until the site stops answering.\n"+
			"Pass the missing hostname(s) again — --domain for the API, --auth-domain for the JWT auth server — or re-run with --deployment <id>, which adopts every hostname the last deploy recorded.\n"+
			"If you do mean to drop one, remove it first and this deploy will stop guarding it:\n"+
			"    kubectl -n %s delete ingress <name>",
		release, namespace, len(live), strings.Join(live, ", "), len(planned), keeping, namespace)
}

// writeImageValues writes an image block at the given indent.
//
// Exactly one of tag and digest is set and the other explicitly cleared: the
// chart prefers a digest when present, so leaving a stale tag alongside it
// would make which one wins depend on what a previous release happened to set.
func writeImageValues(b *strings.Builder, indent, imageRef string) {
	fmt.Fprintf(b, "%simage:\n", indent)
	if repo, digest, ok := strings.Cut(imageRef, "@"); ok {
		fmt.Fprintf(b, "%s  repository: %q\n", indent, repo)
		fmt.Fprintf(b, "%s  digest: %q\n", indent, digest)
		fmt.Fprintf(b, "%s  tag: \"\"\n", indent)
		return
	}
	repo := imageRepoFromRef(imageRef)
	tag := strings.TrimPrefix(strings.TrimPrefix(imageRef, repo), ":")
	fmt.Fprintf(b, "%s  repository: %q\n", indent, repo)
	fmt.Fprintf(b, "%s  tag: %q\n", indent, tag)
	fmt.Fprintf(b, "%s  digest: \"\"\n", indent)
}

// writeIngressValues enables one of the chart's Ingress objects on a hostname.
//
// key selects which — "ingress" for the HTTP one, "grpcIngress" for the gRPC
// one. They are separate objects because backend-protocol is an annotation on
// the object; see k8sValuesYAML.
//
// An empty host writes nothing, and the chart defaults every Ingress to
// disabled, so a host that is not resolved is not served. guardIngressRemoval
// is what stops that from silently deleting one that already exists.
func writeIngressValues(b *strings.Builder, indent, host, key string) {
	if host == "" {
		return
	}
	fmt.Fprintf(b, "%s%s:\n", indent, key)
	fmt.Fprintf(b, "%s  enabled: true\n", indent)
	fmt.Fprintf(b, "%s  hosts:\n", indent)
	fmt.Fprintf(b, "%s    - host: %q\n", indent, host)
	fmt.Fprintf(b, "%s      paths:\n", indent)
	fmt.Fprintf(b, "%s        - path: /\n", indent)
	fmt.Fprintf(b, "%s          pathType: Prefix\n", indent)
}

// writeFileCmd writes content to a path on the host via a quoted heredoc, so
// the remote shell performs no expansion on the body.
// hasRemoteFile reports whether a path exists on the host as a regular file.
//
// Used to decide whether the chart carries a values-custom.yaml: every chart
// generated before that file existed does not, and passing helm a `-f` for a
// missing file is a hard error, so absence has to mean "skip" rather than
// "fail".
func hasRemoteFile(ctx context.Context, st *deployState, path string) bool {
	_, err := st.runner.Capture(ctx, "test -f "+shellQuoteArg(path)+" && echo found")
	return err == nil
}

func writeFileCmd(path, content string) string {
	const delim = "NUZUR_EOF"
	return fmt.Sprintf("mkdir -p %s && cat > %s <<'%s'\n%s\n%s",
		filepath.Dir(path), path, delim, strings.TrimRight(content, "\n"), delim)
}

func shortSHA(sha string) string {
	if len(sha) > 7 {
		return sha[:7]
	}
	return sha
}

// imageRepoFromRef returns the repository part of a container image reference,
// i.e. everything before the tag or digest.
//
// Splitting on the first ":" is wrong and quietly so: a registry may carry a
// port, and "registry.local:5000/app" would yield the repository
// "registry.local" and the tag "5000/app". The tag separator is the LAST colon,
// and only when no "/" follows it — otherwise that colon is the port and the
// reference has no tag at all.
func imageRepoFromRef(ref string) string {
	ref = strings.TrimSpace(ref)
	// A digest wins: "repo@sha256:..." has a colon inside the digest.
	if repo, _, ok := strings.Cut(ref, "@"); ok {
		return repo
	}
	i := strings.LastIndex(ref, ":")
	if i < 0 || strings.Contains(ref[i+1:], "/") {
		return ref
	}
	return ref[:i]
}
