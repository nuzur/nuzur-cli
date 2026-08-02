package app

import (
	"bytes"
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	nemgen "github.com/nuzur/nem/idl/gen"
	"github.com/nuzur/nuzur-cli/constants"
	"github.com/nuzur/nuzur-cli/deploy"
	"github.com/nuzur/nuzur-cli/files"
	"github.com/nuzur/nuzur-cli/localize"
	"github.com/nuzur/nuzur-cli/outputtools"
	"github.com/nuzur/nuzur-cli/productclient"
	pb "github.com/nuzur/nuzur-cli/protodeps/gen"
	"github.com/urfave/cli"
)

// deploy_golden_test.go drives whole `deploy` and `destroy` runs against the
// fakes and compares everything the CLI said, byte for byte, with a checked-in
// transcript.
//
// WHY A TRANSCRIPT AND NOT ASSERTIONS. The deploy pipeline's bugs were almost
// never a wrong return value. They were a right value described wrongly: a
// blocked migration reported as a failed one, a database that had received
// nothing reported as possibly half-migrated, "server cleaned up" printed of a
// box that never answered, a second VM created and billed in silence. What the
// user is told IS the behaviour, so the transcript is the unit under test, and a
// refactor that changes one line of it has changed the product.
//
// FORMAT. One file per scenario, `.golden`, under testdata/goldens. Every line
// is prefixed `OUT ` or `ERR ` by the stream it was written to — the split
// matters (stdout stays machine-parseable, progress goes to stderr) and would be
// invisible in a merged capture. ANSI colour codes are kept: colour is part of
// how loud a message is, and the deploy deliberately reds some lines and yellows
// others. The last line is `EXIT n`.
//
// WHAT IS NOT IN A GOLDEN. Only what the CLI itself authors, because that is all
// that goes through outputtools. The live box output the SSH runner streams, and
// the extension client's own "Execution in progress:" lines (which write to
// os.Stderr directly), are absent here and present in the live transcripts under
// demo/demo2/bugs/transcripts — those remain the specification for the parts the
// fakes cannot produce.

var updateGoldens = flag.Bool("update", false,
	"rewrite the .golden files in app/testdata/goldens from the current output")

// goldenDir is resolved ONCE, from the package directory.
//
// It has to be absolute: the harness moves the process into a temp working
// directory (a deploy with no --source-dir generates into ./nuzur-<identifier>),
// so a relative "testdata/goldens" would read and — under -update — write inside
// the scenario's own scratch space, which is deleted when the test ends.
var goldenDir = sync.OnceValue(func() string {
	wd, err := os.Getwd()
	if err != nil {
		panic("golden tests cannot resolve the package directory: " + err.Error())
	}
	return filepath.Join(wd, "testdata", "goldens")
})

// ── output capture ───────────────────────────────────────────────────────────

// outputLog is the tagged, ordered capture of both output streams.
//
// Tagging happens per LINE rather than per write, because a single
// PrintlnColoredErr can carry an embedded newline (the destructive plan is
// rendered as one multi-line write) and a golden with one 40-line "line" in it
// is not reviewable. Ordering across the two streams is exact: runDeploy is a
// single goroutine, so the sequence of writes is the sequence of events. The
// mutex is there for the interrupt handler's goroutine, which never runs in a
// test but is not worth a race detector finding.
type outputLog struct {
	mu    sync.Mutex
	lines []string

	// A write that does not end in a newline leaves a partial line, held until
	// it is completed. Held per tag, and flushed if the OTHER stream writes
	// first, so an unterminated line can never swallow the line after it.
	pendTag string
	pend    bytes.Buffer
}

// writer returns an io.Writer that tags everything written to it.
func (l *outputLog) writer(tag string) io.Writer { return taggedWriter{log: l, tag: tag} }

type taggedWriter struct {
	log *outputLog
	tag string
}

func (w taggedWriter) Write(p []byte) (int, error) {
	w.log.write(w.tag, p)
	return len(p), nil
}

func (l *outputLog) write(tag string, p []byte) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.pendTag != "" && l.pendTag != tag {
		l.flushLocked()
	}
	l.pendTag = tag
	l.pend.Write(p)
	rest := l.pend.String()
	for {
		idx := strings.IndexByte(rest, '\n')
		if idx < 0 {
			break
		}
		l.lines = append(l.lines, tag+" "+rest[:idx])
		rest = rest[idx+1:]
	}
	l.pend.Reset()
	l.pend.WriteString(rest)
	if rest == "" {
		l.pendTag = ""
	}
}

func (l *outputLog) flushLocked() {
	if l.pend.Len() == 0 {
		return
	}
	l.lines = append(l.lines, l.pendTag+" "+l.pend.String())
	l.pend.Reset()
	l.pendTag = ""
}

// text renders the transcript, flushing any unterminated line.
func (l *outputLog) text() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.flushLocked()
	if len(l.lines) == 0 {
		return ""
	}
	return strings.Join(l.lines, "\n") + "\n"
}

// ── normalization ────────────────────────────────────────────────────────────

var uuidPattern = regexp.MustCompile(`[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}`)

// shortIDPattern is the eight hex characters shortID() appends to an identifier
// to make a deployment id.
var shortIDPattern = regexp.MustCompile(`-[0-9a-f]{8}\b`)

// fixedUUIDs are the uuids the fakes hand out. They are LEFT ALONE by
// normalization: they are already stable, and blanking them would erase exactly
// the information a golden is read for — which agent paired, which project the
// data-manager link points at, whether the connection on the report is the one
// the record carries.
//
// Everything else matching the uuid shape is normalized, which in practice means
// the connection uuid: it is minted with uuid.NewV4() on a first deploy and is
// the only genuinely random uuid the pipeline produces.
func fixedUUIDs() map[string]bool {
	return map[string]bool{
		fakeAgentUUID:          true,
		fakeSecondAgentUUID:    true,
		fakeUserUUID:           true,
		fakeProjectUUID:        true,
		fakeProjectVersionUUID: true,
		fakeTeamUUID:           true,
		fakeGoCodeGenExtUUID:   true,
		fakeGoCodeGenVerUUID:   true,
		fakeSQLPushExtUUID:     true,
		fakeSQLPushVerUUID:     true,
		fakeSQLGenExtUUID:      true,
		fakeSQLGenVerUUID:      true,
		fakeSeedConnUUID:       true,
		fakeSiblingConnUUID:    true,
	}
}

// normalizeGolden removes what a deploy legitimately invents: uuids, the
// deployment short id, the test's temp directory, and the CLI's own version.
// Nothing else — every other value in a golden comes from a fake and is stable by
// construction, so a diff anywhere else is a real change.
//
// The version is the one rule that is not about a run inventing a value: the
// release probe names the CLI's version and the release URL it derives from it
// (deploy_release_probe.go), so without this every `deploy --plan`-free release
// bump would fail two goldens for a reason having nothing to do with the change
// being shipped. `v` is required in the match so a bare "1.5.4" appearing inside
// rendered SQL or a project name is left alone.
func normalizeGolden(s string, tmpDirs []string) string {
	s = strings.ReplaceAll(s, "v"+constants.CLI_VERSION, "v<CLIVERSION>")

	fixed := fixedUUIDs()
	s = uuidPattern.ReplaceAllStringFunc(s, func(m string) string {
		if fixed[m] {
			return m
		}
		return "<UUID>"
	})

	// The short id is normalized only where a deployment id is printed. A
	// blanket rule would rewrite anything that happens to end in eight hex
	// characters — including, on a bad day, part of a rendered SQL statement.
	lines := strings.Split(s, "\n")
	for i, line := range lines {
		if strings.Contains(line, "deployment id:") || strings.Contains(line, "teardown:") {
			lines[i] = shortIDPattern.ReplaceAllString(line, "-<ID8>")
		}
	}
	s = strings.Join(lines, "\n")

	for _, dir := range tmpDirs {
		if dir == "" {
			continue
		}
		s = strings.ReplaceAll(s, dir, "<TMP>")
	}
	return s
}

// ── the harness ──────────────────────────────────────────────────────────────

// Fixed identities used by seeded records, so a golden that names a connection
// or an agent names the same one every run.
const (
	fakeSeedConnUUID    = "f8888e33-0000-0000-0000-0000000000c1"
	fakeSiblingConnUUID = "f8888e33-0000-0000-0000-0000000000c2"

	// seedDeploymentID / seedResourceName describe the box a re-deploy,
	// --new-vm run or destroy finds already recorded. The id deliberately does
	// NOT look like `<identifier>-<8 hex>`, so the short-id normalization cannot
	// touch it and a golden shows the real recorded id.
	seedDeploymentID    = "sfapi-seedbox0"
	seedResourceName    = "nuzur-sfapi-seed01"
	siblingDeploymentID = "billing-seedbox1"
)

// goldenEnv describes one scenario: what the user typed, what was already on
// this machine, and how the outside world behaves.
type goldenEnv struct {
	// command is "deploy" (the default) or "destroy".
	command string
	// args is the command line after the command name, exactly as typed. It is
	// also what os.Args is set to, so the rerun suggestions the destructive gate
	// prints are the ones the user would actually be given.
	args []string

	// seed writes deployment records before the run, as if earlier deploys had
	// happened on this machine. It is a function because a record's WorkspaceDir
	// has to point inside the test's own working directory.
	seed func(work string) []*deploy.Deployment

	// The five fakes, each given a chance to script itself. Defaults are the
	// ordinary successful run.
	product func(*fakeProductClient)
	er      func(*fakeExtensionRunner)
	ssh     func(*fakeRemoteRunner)
	prov    func(*fakeProvisioner)
	http    func(*fakeRoundTripper)
}

// goldenRun is what a scenario produced, for the record-store assertions each
// test makes on top of the transcript comparison.
type goldenRun struct {
	home string
	work string

	imp     *Implementation
	product *fakeProductClient
	er      *fakeExtensionRunner
	ssh     *fakeRemoteRunner
	prov    *fakeProvisioner
	http    *fakeRoundTripper

	// transcript is the normalized golden text, exit line included.
	transcript string
	exit       int
	err        error
}

// deployments reads back every record on the isolated machine, by id.
func (g *goldenRun) deployments(t *testing.T) map[string]deploy.Deployment {
	t.Helper()
	deps, err := deploy.ListDeployments()
	if err != nil {
		t.Fatalf("listing deployments: %v", err)
	}
	out := map[string]deploy.Deployment{}
	for _, d := range deps {
		out[d.ID] = d
	}
	return out
}

// recordJSON is the raw file a record was written as, for the scenarios whose
// claim is that a record was not touched AT ALL. Reading the struct proves the
// fields a test thought to name; reading the bytes proves the file.
func (g *goldenRun) recordJSON(t *testing.T, id string) string {
	t.Helper()
	raw, err := os.ReadFile(files.DeploymentFilePath(id))
	if err != nil {
		t.Fatalf("reading the record file for %s: %v", id, err)
	}
	return string(raw)
}

// onlyDeployment reads back the single record the run should have left.
func (g *goldenRun) onlyDeployment(t *testing.T) deploy.Deployment {
	t.Helper()
	deps := g.deployments(t)
	if len(deps) != 1 {
		t.Fatalf("expected exactly one deployment record, got %d: %v", len(deps), deps)
	}
	for _, d := range deps {
		return d
	}
	return deploy.Deployment{}
}

// runDeployGolden drives one scenario end to end and compares its transcript
// with testdata/goldens/<name>.golden.
//
// The sequence matters at two points. The output writers are swapped BEFORE the
// Implementation is built, because the production SSH accessor snapshots
// outputtools.Stderr into every runner it makes. And the working directory is
// moved into the isolated home before anything resolves a workspace, because a
// deploy with no --source-dir generates into ./nuzur-<identifier>.
func runDeployGolden(t *testing.T, name string, env goldenEnv) *goldenRun {
	t.Helper()
	// t.Setenv (inside isolateHome) already forbids it, but the reason is worth
	// stating where it is load-bearing: this test swaps process-wide state —
	// HOME, os.Args, the working directory and both output sinks.
	// Resolved before anything moves the working directory.
	_ = goldenDir()
	home := isolateHome(t)
	// The CLI picks its language from $LANG (outputtools.GetLocale), and it
	// ships a Spanish bundle. Without this a golden authored on an English
	// machine fails on a Spanish one, for a reason nothing in the diff explains.
	t.Setenv("LANG", "en_US.UTF-8")
	work := chdirTemp(t, filepath.Join(home, "work"))

	command := env.command
	if command == "" {
		command = "deploy"
	}

	if env.seed != nil {
		for _, rec := range env.seed(work) {
			if err := deploy.SaveDeployment(rec); err != nil {
				t.Fatalf("seeding deployment %s: %v", rec.ID, err)
			}
		}
	}

	log := &outputLog{}
	swapOutputWriters(t, log.writer("OUT"), log.writer("ERR"))
	setArgv(t, append([]string{"nuzur-cli", command}, env.args...)...)

	product := newFakeProductClient()
	if env.product != nil {
		env.product(product)
	}
	er := newFakeExtensionRunner()
	if env.er != nil {
		env.er(er)
	}
	ssh := newFakeRemoteRunner()
	if env.ssh != nil {
		env.ssh(ssh)
	}
	prov := newFakeProvisioner(deploy.ProviderDigitalOcean)
	if env.prov != nil {
		env.prov(prov)
	}
	rt := newFakeRoundTripper()
	if env.http != nil {
		env.http(rt)
	}

	imp := &Implementation{
		localize:      localize.New(),
		productClient: &productclient.Client{ProductClient: product},

		newSSHRunner: ssh.factory,
		newProvisioner: func(p deploy.Provider) (deploy.Provisioner, error) {
			prov.Provider = p
			return prov, nil
		},
		newExtensionRunner: func() (extensionRunner, error) { return er, nil },
		httpTransport:      rt,
	}
	// The login line, reproduced rather than skipped. Every command prints it
	// and it is the first line of every live transcript, so a golden without it
	// would start one line later than reality. auth.Login cannot be used: it
	// builds its own product client and dials nuzur for the token's user (see
	// auth/token.go), which is precisely what the loginFn seam exists to avoid —
	// so this is auth.LoginStatus's body with the injected client in its place.
	imp.loginFn = func() error {
		authCtx, err := productclient.ClientContext()
		if err != nil {
			return err
		}
		user, err := product.GetTokenUser(authCtx, &pb.GetTokenUserRequest{})
		if err != nil {
			outputtools.PrintlnColoredErr(imp.localize.Localize("logged_out", "Logged out"), outputtools.Red)
			return err
		}
		outputtools.PrintlnColoredErr(
			fmt.Sprintf("%s [%s - %s]", imp.localize.Localize("logged_in", "Logged in as"), user.GetName(), user.GetEmail()),
			outputtools.Green)
		return nil
	}

	var runErr error
	switch command {
	case "deploy":
		// Through the command's own Action, not straight into runDeploy: the
		// argv guard lives there, and it is one of the scenarios.
		action, ok := imp.DeployCommand().Action.(func(*cli.Context) error)
		if !ok {
			t.Fatal("DeployCommand().Action is not a func(*cli.Context) error")
		}
		runErr = action(deployContext(t, env.args))
	case "destroy":
		cmd := imp.DestroyCommand()
		action, ok := cmd.Action.(func(*cli.Context) error)
		if !ok {
			t.Fatal("DestroyCommand().Action is not a func(*cli.Context) error")
		}
		runErr = action(flagContext(t, cmd.Flags, env.args))
	default:
		t.Fatalf("unknown command %q", command)
	}

	exit := renderTerminalError(log.writer("ERR"), runErr)
	// Both spellings of the temp home: t.TempDir() hands back the unresolved
	// path (/var/folders/… on darwin) while os.Getwd() — and therefore every
	// absolute path the CLI derives from the working directory — reports the
	// resolved one (/private/var/folders/…).
	tmpDirs := []string{home}
	if resolved, err := filepath.EvalSymlinks(home); err == nil && resolved != home {
		tmpDirs = append([]string{resolved}, tmpDirs...)
	}
	transcript := normalizeGolden(log.text(), tmpDirs) + fmt.Sprintf("EXIT %d\n", exit)

	compareGolden(t, name, transcript)

	return &goldenRun{
		home: home, work: work,
		imp: imp, product: product, er: er, ssh: ssh, prov: prov, http: rt,
		transcript: transcript, exit: exit, err: runErr,
	}
}

// renderTerminalError writes what the process would have printed about a
// returned error, and returns the exit code it would have exited with.
//
// This reproduces the two layers above the seam, which a test calling the
// command's Action directly does not go through:
//
//   - urfave/cli's HandleExitCoder (command.go calls it on every error): an
//     ExitCoder's message is printed to stderr ONLY when non-empty, then the
//     process exits with its code. cli.NewExitError("", 1) is the CLI's way of
//     saying "everything worth saying has already been printed, just fail" —
//     which is why the gate scenarios end with a bare EXIT 1.
//   - main.go's log.Fatal for everything else: the message on stderr, exit 1.
//     log's timestamp prefix is deliberately NOT reproduced; it is the one thing
//     in that line that cannot be stable, and it carries no information a golden
//     would be read for.
func renderTerminalError(w io.Writer, err error) int {
	if err == nil {
		return 0
	}
	if exitErr, ok := err.(cli.ExitCoder); ok {
		if msg := err.Error(); msg != "" {
			fmt.Fprintln(w, msg)
		}
		return exitErr.ExitCode()
	}
	fmt.Fprintln(w, err.Error())
	return 1
}

// compareGolden checks the transcript against the checked-in file, or rewrites
// it under -update.
func compareGolden(t *testing.T, name, got string) {
	t.Helper()
	path := filepath.Join(goldenDir(), name+".golden")
	if *updateGoldens {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatalf("creating the goldens dir: %v", err)
		}
		if err := os.WriteFile(path, []byte(got), 0o644); err != nil {
			t.Fatalf("writing %s: %v", path, err)
		}
		return
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading %s (run `go test ./app -run Golden -update` to author it): %v", path, err)
	}
	if string(want) == got {
		return
	}
	t.Errorf("transcript differs from %s:\n%s", path, unifiedish(string(want), got))
}

// unifiedish renders the first differing lines of two transcripts. Not a real
// diff: a golden is a few dozen lines and the first divergence is almost always
// the whole story, while a full diff of ANSI-laden text is unreadable.
func unifiedish(want, got string) string {
	wantLines := strings.Split(want, "\n")
	gotLines := strings.Split(got, "\n")
	var b strings.Builder
	shown := 0
	for i := 0; i < len(wantLines) || i < len(gotLines); i++ {
		var w, g string
		if i < len(wantLines) {
			w = wantLines[i]
		}
		if i < len(gotLines) {
			g = gotLines[i]
		}
		if w == g {
			continue
		}
		fmt.Fprintf(&b, "line %d:\n  want: %q\n  got:  %q\n", i+1, w, g)
		shown++
		if shown >= 8 {
			b.WriteString("  (further differences elided)\n")
			break
		}
	}
	if shown == 0 {
		b.WriteString("(only trailing content differs)\n")
	}
	return b.String()
}

// flagContext builds a *cli.Context from a command's REAL flag list, the way
// deployContext does for deploy — derived rather than mirrored, so a flag added
// to the command is reachable from a test the moment it exists.
func flagContext(t *testing.T, flags []cli.Flag, args []string) *cli.Context {
	t.Helper()
	set := flag.NewFlagSet("cmd", flag.ContinueOnError)
	for _, f := range flags {
		f.Apply(set)
	}
	if err := set.Parse(args); err != nil {
		t.Fatalf("parsing args: %v", err)
	}
	return cli.NewContext(nil, set, nil)
}

// ── shared scenario material ─────────────────────────────────────────────────

// seedCreatedAt is the recorded creation time of every seeded deployment. Fixed
// so a re-deploy that preserves it (which it must — the record dates from the
// FIRST deploy of that box) can be asserted on exactly.
var seedCreatedAt = time.Date(2026, 7, 1, 9, 30, 0, 0, time.UTC)

// savedGoCodeGenConfig is the go-code-gen config of a project that has run the
// generator before. Its presence is what makes a deploy skip the "deploying with
// derived defaults" notice, and its storage_enabled is what makes one print the
// S3-credentials warning — both of which the live transcripts show.
func savedGoCodeGenConfig() map[string]interface{} {
	return map[string]interface{}{
		"identifier":          "sfapi",
		"go_module":           "sfapi",
		"db":                  "mysql",
		"events":              "disabled",
		"auth":                "disabled",
		"proto_enabled":       false,
		"grpc_server_enabled": false,
		"rest_enabled":        true,
		"grpc_port":           "6009",
		"http_port":           "8080",
		"dockerfile":          true,
		"storage_enabled":     true,
		"rest_base_path":      "/v1",
	}
}

// deployedRecord is the record a completed managed deploy of `sfapi` leaves: a
// digitalocean box with its instance id, its paired agent and its connection.
func deployedRecord(work string) *deploy.Deployment {
	return &deploy.Deployment{
		ID:                   seedDeploymentID,
		Provider:             deploy.ProviderDigitalOcean,
		ProviderInstanceID:   fakeInstanceID,
		ProviderResourceName: seedResourceName,
		Region:               fakeRegion,
		Host:                 fakeProvisionedHost,
		User:                 "root",
		Port:                 22,
		Identifier:           "sfapi",
		ProjectUUID:          fakeProjectUUID,
		ProjectVersionUUID:   fakeProjectVersionUUID,
		LocalAgentUUID:       fakeAgentUUID,
		ConnUUID:             fakeSeedConnUUID,
		DBEngine:             deploy.DBMySQL,
		WorkspaceDir:         filepath.Join(work, "nuzur-sfapi"),
		APIURL:               "http://" + fakeProvisionedHost + ":8443",
		PublicURL:            "http://" + fakeProvisionedHost + ":8443",
		CreatedAt:            seedCreatedAt,
	}
}

// midProvisionRecord is what a deploy killed during the provider's create call
// leaves behind: a reserved resource name, no host, and Provisioning still set.
// It is the record decideDeployBox refuses to provision past.
func midProvisionRecord() *deploy.Deployment {
	return &deploy.Deployment{
		ID:                   seedDeploymentID,
		Provider:             deploy.ProviderDigitalOcean,
		ProviderResourceName: seedResourceName,
		Provisioning:         true,
		Region:               fakeRegion,
		Identifier:           "sfapi",
		ProjectUUID:          fakeProjectUUID,
		ProjectVersionUUID:   fakeProjectVersionUUID,
		DBEngine:             deploy.DBMySQL,
		CreatedAt:            seedCreatedAt,
	}
}

// diedInFlightRecord is what a deploy killed AFTER its box was recorded and
// BEFORE its agent paired leaves behind: a real, reachable, billing VM, and a
// record that says exactly that — how far the run got, and what it stopped on.
//
// Distinct from midProvisionRecord, which never learned a host and so leaves
// nothing to reach. This one has everything needed to finish the job, which is
// why the next run adopts it rather than billing for a second VM beside it.
func diedInFlightRecord(work string) *deploy.Deployment {
	rec := deployedRecord(work)
	// The fields steps 9 and 12 fill in are precisely the ones it never reached.
	rec.LocalAgentUUID = ""
	rec.APIURL = ""
	rec.PublicURL = ""
	rec.LastCompletedStep = deploy.StepBoxRecorded
	rec.LastError = "remote bootstrap script failed: exit status 1"
	return rec
}

// managedDeployArgs is the invocation every managed-deploy scenario starts from.
func managedDeployArgs(extra ...string) []string {
	return append([]string{
		"--provider", "digitalocean",
		"--project", "sfapi",
		"--version", "v_21",
		"--identifier", "sfapi",
		"--api", "both",
		"--auth", "jwt",
	}, extra...)
}

// firstDeployAgents scripts the agent poll for a deploy that pairs a NEW agent:
// absent when runDeploy snapshots, ONLINE by the time it waits.
func firstDeployAgents(f *fakeProductClient) {
	f.AgentsByCall = [][]*nemgen.LocalAgent{
		{},
		{onlineAgent(fakeAgentUUID)},
	}
}

// The two migrations the scenarios present at sql-push's confirmation step.
const (
	additiveSQL = "CREATE TABLE `invoice` (\n" +
		"  `uuid` char(36) NOT NULL,\n" +
		"  `total` decimal(12,2) NOT NULL,\n" +
		"  PRIMARY KEY (`uuid`)\n" +
		");\n" +
		"ALTER TABLE `customer` ADD COLUMN `vat_number` varchar(32) NULL;\n"

	destructiveSQL = "ALTER TABLE `customer` ADD COLUMN `vat_number` varchar(32) NULL;\n" +
		"ALTER TABLE `customer` DROP COLUMN `legacy_ref`;\n" +
		"DROP TABLE `audit_log_2024`;\n"
)

// ── the scenarios ────────────────────────────────────────────────────────────
//
// Each is one live-transcript shape, reduced to what the CLI itself says. Every
// deploy scenario also asserts what it left in the record store: the transcript
// is what the user was told, the record is what `destroy` will act on, and the
// bugs this suite is a regression bed for are precisely the ones where those two
// disagreed.

// A managed first deploy: no record on this machine, so a VM is created, a NEW
// agent pairs, and the record goes from "provisioning" to fully populated.
//
// Reference: r6/deploy1b.log.
func TestGoldenFirstManagedDeploy(t *testing.T) {
	g := runDeployGolden(t, "first_managed_deploy", goldenEnv{
		args:    managedDeployArgs(),
		product: firstDeployAgents,
		er: func(f *fakeExtensionRunner) {
			f.setLastGoCodeGenConfig(savedGoCodeGenConfig())
		},
	})

	if g.exit != 0 {
		t.Errorf("exit = %d, want 0", g.exit)
	}
	rec := g.onlyDeployment(t)
	// The four things only the END of a deploy knows, and the ones destroy and
	// `--plan --deployment` are useless without.
	if rec.ProviderInstanceID != fakeInstanceID {
		t.Errorf("ProviderInstanceID = %q, want the created instance %q", rec.ProviderInstanceID, fakeInstanceID)
	}
	if rec.ProviderResourceName == "" {
		t.Error("ProviderResourceName is empty — the only handle on a VM whose id never came back")
	}
	if rec.LocalAgentUUID != fakeAgentUUID {
		t.Errorf("LocalAgentUUID = %q, want the paired agent %q", rec.LocalAgentUUID, fakeAgentUUID)
	}
	if rec.APIURL == "" || rec.PublicURL == "" || rec.DataManagerURL == "" {
		t.Errorf("URLs not finalized: api=%q public=%q dm=%q", rec.APIURL, rec.PublicURL, rec.DataManagerURL)
	}
	if rec.Provisioning {
		t.Error("Provisioning is still set on a finished deploy — destroy would skip the on-box teardown")
	}
	if rec.Host != fakeProvisionedHost || rec.Region != fakeRegion {
		t.Errorf("box not recorded: host=%q region=%q", rec.Host, rec.Region)
	}
	if rec.WorkspaceDir == "" {
		t.Error("WorkspaceDir is empty — the next deploy would generate somewhere else")
	}
	// How far this run got, stated rather than inferred. The ORDER the checkpoints
	// were written in is asserted separately, by
	// TestDeployRecordSequenceManagedFirstDeploy.
	if rec.LastCompletedStep != deploy.StepFinalized {
		t.Errorf("LastCompletedStep = %q, want %q", rec.LastCompletedStep, deploy.StepFinalized)
	}
	if rec.LastError != "" {
		t.Errorf("a clean deploy recorded LastError = %q", rec.LastError)
	}

	// The pre-provision write is what makes the VM destroyable for the whole
	// create call. It is invisible in the transcript, so it is asserted here.
	if got := len(g.product.CallsTo("IssueProvisioningToken")); got != 1 {
		t.Errorf("IssueProvisioningToken called %d times, want 1", got)
	}
	if calls := g.prov.Calls(); len(calls) == 0 || !strings.HasPrefix(calls[0], "Provision ") {
		t.Errorf("provider calls = %v, want a Provision first", calls)
	}
}

// A managed re-deploy onto the box the previous one created: no VM, the same
// agent, the same record — and the pre-flight gate runs before anything ships.
//
// Reference: r6/deploy2_reuse.log (its clean half).
func TestGoldenRedeployReuseClean(t *testing.T) {
	g := runDeployGolden(t, "redeploy_reuse_clean", goldenEnv{
		args: managedDeployArgs(),
		seed: func(work string) []*deploy.Deployment {
			return []*deploy.Deployment{deployedRecord(work)}
		},
		er: func(f *fakeExtensionRunner) {
			f.setLastGoCodeGenConfig(savedGoCodeGenConfig())
		},
	})

	if g.exit != 0 {
		t.Errorf("exit = %d, want 0", g.exit)
	}
	rec := g.onlyDeployment(t)
	if rec.ID != seedDeploymentID {
		t.Errorf("re-deploy minted a new record %q; it must update %q in place", rec.ID, seedDeploymentID)
	}
	if !rec.CreatedAt.Equal(seedCreatedAt) {
		t.Errorf("CreatedAt = %s, want the original %s — the record dates from the first deploy of this box", rec.CreatedAt, seedCreatedAt)
	}
	if rec.ConnUUID != fakeSeedConnUUID {
		t.Errorf("ConnUUID = %q, want the preserved %q", rec.ConnUUID, fakeSeedConnUUID)
	}
	if rec.LocalAgentUUID != fakeAgentUUID {
		t.Errorf("LocalAgentUUID = %q, want the reused agent", rec.LocalAgentUUID)
	}
	if rec.ProviderInstanceID != fakeInstanceID || rec.ProviderResourceName != seedResourceName {
		t.Errorf("provider handles lost on re-deploy: id=%q name=%q", rec.ProviderInstanceID, rec.ProviderResourceName)
	}
	// Nothing was PROVISIONED: the whole point of the reuse. (The provider
	// firewall is deliberately re-applied on a reused box — a re-deploy can open
	// a new project's port — so the provider is not untouched, only the create
	// call is.)
	if containsPrefix(g.prov.Calls(), "Provision ") {
		t.Errorf("a reuse provisioned a VM: %v", g.prov.Calls())
	}
	if rec.LastCompletedStep != deploy.StepFinalized || rec.LastError != "" {
		t.Errorf("checkpoint after a clean re-deploy = %q / %q, want %q / \"\"",
			rec.LastCompletedStep, rec.LastError, deploy.StepFinalized)
	}
	// The first-deploy render pre-check does NOT run here, and its whole cost is
	// this call. A re-deploy has a live database and the pre-flight gate above
	// diffs against it; paying for a second, redundant generator run on every
	// re-deploy is the failure mode the skip exists to avoid.
	for _, call := range g.er.Calls() {
		if strings.HasPrefix(call, "GetStandaloneEntities") || call == "Run "+sqlGenExtensionIdentifier {
			t.Errorf("a re-deploy ran the first-deploy schema pre-check (%q): %v", call, g.er.Calls())
		}
	}
}

// The same re-deploy, with the schema apply failing at extension resolution.
//
// This is r6/deploy2_reuse.log as it actually ran: the pre-flight gate computed
// its plan fine, the deploy shipped, and the apply then timed out RESOLVING
// sql-push-local — before a single statement was sent. The transcript predates
// the fix, so it reports a migration that may have landed half-applied; the
// current CLI says the database was never touched, which is what this pins.
func TestGoldenRedeployApplyTimeout(t *testing.T) {
	timeout := fmt.Errorf("rpc error: code = DeadlineExceeded desc = context deadline exceeded")
	g := runDeployGolden(t, "redeploy_apply_timeout", goldenEnv{
		args: managedDeployArgs(),
		seed: func(work string) []*deploy.Deployment {
			return []*deploy.Deployment{deployedRecord(work)}
		},
		er: func(f *fakeExtensionRunner) {
			f.setLastGoCodeGenConfig(savedGoCodeGenConfig())
			// nil = the pre-flight resolution succeeds; the apply's does not.
			f.FindExtensionErrs[sqlPushPair.Local] = []error{nil, timeout}
		},
	})

	if g.exit != 1 {
		t.Errorf("exit = %d, want 1 — a schema that did not reach the database must not go green", g.exit)
	}
	// The deploy itself happened, so the record is finalized even though the
	// schema step failed.
	rec := g.onlyDeployment(t)
	if rec.ID != seedDeploymentID || rec.LocalAgentUUID != fakeAgentUUID || rec.APIURL == "" {
		t.Errorf("record not finalized after a failed schema step: %+v", rec)
	}
	// The DEPLOYMENT finished; the schema step did not. The checkpoint describes
	// the former, and LastError stays empty because a schema outcome is reported
	// through the revision and the exit code, not by marking the deployment
	// itself broken.
	if rec.LastCompletedStep != deploy.StepFinalized || rec.LastError != "" {
		t.Errorf("checkpoint = %q / %q, want %q / \"\"", rec.LastCompletedStep, rec.LastError, deploy.StepFinalized)
	}
}

// --new-vm on a project whose box is already recorded and running: a second VM,
// a second record, a second agent — and a warning that both now bill.
//
// Reference: r6/deploy3_newvm.log.
func TestGoldenNewVM(t *testing.T) {
	g := runDeployGolden(t, "new_vm", goldenEnv{
		args: managedDeployArgs("--new-vm"),
		seed: func(work string) []*deploy.Deployment {
			return []*deploy.Deployment{deployedRecord(work)}
		},
		product: func(f *fakeProductClient) {
			// The old box's agent is already paired; the new box's appears next.
			f.AgentsByCall = [][]*nemgen.LocalAgent{
				{onlineAgent(fakeAgentUUID)},
				{onlineAgent(fakeAgentUUID), onlineAgent(fakeSecondAgentUUID)},
			}
		},
		er: func(f *fakeExtensionRunner) {
			f.setLastGoCodeGenConfig(savedGoCodeGenConfig())
		},
	})

	if g.exit != 0 {
		t.Errorf("exit = %d, want 0", g.exit)
	}
	deps := g.deployments(t)
	if len(deps) != 2 {
		t.Fatalf("expected the old record AND a new one, got %d: %v", len(deps), deps)
	}
	old, ok := deps[seedDeploymentID]
	if !ok {
		t.Fatalf("the previous deployment record is gone — its VM is still billing and nothing points at it")
	}
	if old.LocalAgentUUID != fakeAgentUUID || old.ProviderInstanceID != fakeInstanceID {
		t.Errorf("the previous record was modified by a --new-vm run: %+v", old)
	}
	// The seed was written without checkpoint fields, i.e. by a pre-checkpoint
	// CLI. It reads back as "nothing known completed" and is not retro-fitted by
	// a run that had nothing to do with it.
	if old.LastCompletedStep != "" || old.LastError != "" {
		t.Errorf("a --new-vm run wrote checkpoints onto the previous record: step=%q err=%q",
			old.LastCompletedStep, old.LastError)
	}
	for id, rec := range deps {
		if id == seedDeploymentID {
			continue
		}
		if rec.LocalAgentUUID != fakeSecondAgentUUID {
			t.Errorf("the fresh VM recorded agent %q, want its own %q", rec.LocalAgentUUID, fakeSecondAgentUUID)
		}
		if rec.ConnUUID == fakeSeedConnUUID {
			t.Error("the fresh VM reused the old box's connection uuid")
		}
		if rec.LastCompletedStep != deploy.StepFinalized {
			t.Errorf("the fresh VM's checkpoint = %q, want %q", rec.LastCompletedStep, deploy.StepFinalized)
		}
	}
}

// A managed re-deploy whose recorded box does not answer. It stops rather than
// silently provisioning a replacement, and leaves the record exactly as it was.
//
// Reference: r6/deploy4_stale.log — thirteen lines, all of them the CLI's.
func TestGoldenStaleRecordUnreachable(t *testing.T) {
	g := runDeployGolden(t, "stale_record_unreachable", goldenEnv{
		args: managedDeployArgs(),
		seed: func(work string) []*deploy.Deployment {
			return []*deploy.Deployment{deployedRecord(work)}
		},
		er: func(f *fakeExtensionRunner) {
			f.setLastGoCodeGenConfig(savedGoCodeGenConfig())
		},
		ssh: func(r *fakeRemoteRunner) {
			r.PingErrs = []error{fmt.Errorf(
				"ssh preflight to root@%s failed (check host, user, and key): remote command failed: exit status 255",
				fakeProvisionedHost)}
		},
	})

	if g.exit != 1 {
		t.Errorf("exit = %d, want 1", g.exit)
	}
	// Untouched: the record is the only handle on a VM that may still exist, and
	// the user was told to destroy it by that id.
	rec := g.onlyDeployment(t)
	want := deployedRecord(g.work)
	if rec.ID != want.ID || rec.Host != want.Host || rec.LocalAgentUUID != want.LocalAgentUUID ||
		rec.ConnUUID != want.ConnUUID || rec.ProviderInstanceID != want.ProviderInstanceID ||
		!rec.CreatedAt.Equal(want.CreatedAt) {
		t.Errorf("the seeded record was modified by a refused deploy:\n got %+v\nwant %+v", rec, *want)
	}
	// Untouched at the byte level, not merely in the fields this test names. A
	// deploy that refuses before the box exists has no checkpoint to write and no
	// error to attribute to THIS record — the record it stopped on belongs to an
	// earlier, possibly still-running deployment.
	if raw := g.recordJSON(t, seedDeploymentID); strings.Contains(raw, "last_completed_step") ||
		strings.Contains(raw, "last_error") {
		t.Errorf("a refused deploy annotated the record it declined to touch:\n%s", raw)
	}
	// Nothing was generated, provisioned or reported.
	if calls := g.prov.Calls(); len(calls) != 0 {
		t.Errorf("provider was called after the box refused to answer: %v", calls)
	}
	if calls := g.product.CallsTo("UpsertDeployment"); len(calls) != 0 {
		t.Errorf("a refused deploy was reported to nuzur: %v", calls)
	}
}

// --new-vm as RECOVERY: the previous deploy died during the provider's create
// call, so its record has a reserved name and no host at all.
//
// This is the case decideDeployBox's conditional --new-vm wording was written
// for (R6#8). r6/deploy5_recover.log carries the pre-fix text and the arm where
// the old box HAS a host; this scenario drives the other arm, the one that can
// say nothing about a server it never learned the address of.
func TestGoldenRecoverNewVM(t *testing.T) {
	g := runDeployGolden(t, "recover_new_vm", goldenEnv{
		args: managedDeployArgs("--new-vm"),
		seed: func(work string) []*deploy.Deployment {
			return []*deploy.Deployment{midProvisionRecord()}
		},
		product: firstDeployAgents,
		er: func(f *fakeExtensionRunner) {
			f.setLastGoCodeGenConfig(savedGoCodeGenConfig())
		},
	})

	if g.exit != 0 {
		t.Errorf("exit = %d, want 0", g.exit)
	}
	deps := g.deployments(t)
	if len(deps) != 2 {
		t.Fatalf("expected the orphan record AND a new one, got %d: %v", len(deps), deps)
	}
	orphan, ok := deps[seedDeploymentID]
	if !ok {
		t.Fatal("the mid-provision record is gone — destroy can no longer find the VM it reserved")
	}
	if !orphan.Provisioning || orphan.ProviderResourceName != seedResourceName {
		t.Errorf("the orphan record was altered: %+v", orphan)
	}
	if orphan.LastCompletedStep != "" || orphan.LastError != "" {
		t.Errorf("the orphan record was annotated by an unrelated run: step=%q err=%q",
			orphan.LastCompletedStep, orphan.LastError)
	}
	for id, rec := range deps {
		if id == seedDeploymentID {
			continue
		}
		if rec.LastCompletedStep != deploy.StepFinalized {
			t.Errorf("the recovery deploy's checkpoint = %q, want %q", rec.LastCompletedStep, deploy.StepFinalized)
		}
	}
}

// The two-records-for-one-box class, retired end to end: a deploy died after
// recording its box, and the next one ADOPTS that box and that record.
//
// The checkpoint is what makes the run legible rather than merely correct.
// pickPriorDeployment skips the record — it never paired, so there is no agent
// and no connection to push a schema through — decideDeployBox reuses the box it
// describes, and the reuse line states what the record says instead of leaving
// the user to work out why a server is being reused with no agent on it.
func TestGoldenDiedInFlightAdoption(t *testing.T) {
	g := runDeployGolden(t, "died_in_flight_adoption", goldenEnv{
		args: managedDeployArgs(),
		seed: func(work string) []*deploy.Deployment {
			return []*deploy.Deployment{diedInFlightRecord(work)}
		},
		// Nothing on this machine knows an agent for the box (the run that made
		// it died first), so this deploy pairs one exactly as a first deploy does.
		product: firstDeployAgents,
		er: func(f *fakeExtensionRunner) {
			f.setLastGoCodeGenConfig(savedGoCodeGenConfig())
		},
	})

	if g.exit != 0 {
		t.Errorf("exit = %d, want 0", g.exit)
	}
	// ONE record. A second one for the same box is the whole failure class: it is
	// what makes destroy's isLast refuse to delete a VM that nothing else will.
	rec := g.onlyDeployment(t)
	if rec.ID != seedDeploymentID {
		t.Errorf("the adoption minted a new record %q instead of finishing %q", rec.ID, seedDeploymentID)
	}
	if !rec.CreatedAt.Equal(seedCreatedAt) {
		t.Errorf("CreatedAt = %s, want the adopted record's %s — this deployment started when that run did",
			rec.CreatedAt, seedCreatedAt)
	}
	// Nothing was provisioned: the box in the record IS the box being deployed to.
	if containsPrefix(g.prov.Calls(), "Provision ") {
		t.Errorf("an adoption provisioned a second VM: %v", g.prov.Calls())
	}
	if rec.Host != fakeProvisionedHost || rec.ProviderInstanceID != fakeInstanceID ||
		rec.ProviderResourceName != seedResourceName {
		t.Errorf("the adopted box's handles were lost: host=%q instance=%q name=%q",
			rec.Host, rec.ProviderInstanceID, rec.ProviderResourceName)
	}
	if rec.LocalAgentUUID != fakeAgentUUID {
		t.Errorf("LocalAgentUUID = %q, want the newly paired agent %q", rec.LocalAgentUUID, fakeAgentUUID)
	}
	// A finished deployment now, and the previous run's error is GONE: a stale
	// error on a healthy record is a lie the next run would read as fact.
	if rec.LastCompletedStep != deploy.StepFinalized || rec.LastError != "" {
		t.Errorf("checkpoint after the adoption = %q / %q, want %q / \"\"",
			rec.LastCompletedStep, rec.LastError, deploy.StepFinalized)
	}
	// And in the transcript: the recorded fact, not an inference from an empty
	// field. This is the only golden in which that sentence appears — every other
	// seeded record predates checkpoints.
	for _, want := range []string{
		"died after 'box_recorded'",
		"remote bootstrap script failed: exit status 1",
	} {
		if !strings.Contains(g.transcript, want) {
			t.Errorf("the transcript does not state the recorded checkpoint (%q):\n%s", want, g.transcript)
		}
	}
}

// A re-deploy whose migration would delete data, refused BEFORE anything ships.
//
// The box keeps serving the code that matches its schema, which is the entire
// reason the gate moved up front: refusing at step 10 refuses after the image
// has been rebuilt and the container restarted.
//
// Reference: r4-e2e/deploy-pg-v3-gate.log.
func TestGoldenPreflightGateBlock(t *testing.T) {
	g := runDeployGolden(t, "preflight_gate_block", goldenEnv{
		args: managedDeployArgs(),
		seed: func(work string) []*deploy.Deployment {
			return []*deploy.Deployment{deployedRecord(work)}
		},
		er: func(f *fakeExtensionRunner) {
			f.setLastGoCodeGenConfig(savedGoCodeGenConfig())
			f.SQLPlan = destructiveSQL
		},
	})

	if g.exit != 1 {
		t.Errorf("exit = %d, want 1", g.exit)
	}
	// Nothing shipped: no generation, no bootstrap, no report, and the record is
	// as it was.
	for _, call := range g.ssh.Calls() {
		if strings.HasPrefix(call, "RunScript") || strings.HasPrefix(call, "CopyDir") {
			t.Errorf("a blocked deploy still touched the box: %v", g.ssh.Calls())
			break
		}
	}
	if calls := g.product.CallsTo("UpsertDeployment"); len(calls) != 0 {
		t.Errorf("a blocked deploy was reported to nuzur: %v", calls)
	}
	rec := g.onlyDeployment(t)
	if !rec.CreatedAt.Equal(seedCreatedAt) || rec.LocalAgentUUID != fakeAgentUUID {
		t.Errorf("the record was rewritten by a blocked deploy: %+v", rec)
	}
	if raw := g.recordJSON(t, seedDeploymentID); strings.Contains(raw, "last_completed_step") ||
		strings.Contains(raw, "last_error") {
		t.Errorf("a blocked deploy annotated the record of the deployment it left running:\n%s", raw)
	}
	// The gate was reached by actually computing the plan, and the plan it
	// rejected is the one it was shown.
	if steps := g.er.Steps(); len(steps) != 1 || steps[0].Content != destructiveSQL {
		t.Errorf("the gate did not see the migration it blocked: %+v", steps)
	}
}

// A schema apply that failed with statements already issued. sql-push confirmed
// the step and then errored, so the deploy cannot say what is in the database —
// and the app on the box has already been rebuilt against the new schema.
func TestGoldenSchemaApplyFailedDuring(t *testing.T) {
	g := runDeployGolden(t, "schema_apply_failed_during", goldenEnv{
		args:    managedDeployArgs(),
		product: firstDeployAgents,
		er: func(f *fakeExtensionRunner) {
			f.setLastGoCodeGenConfig(savedGoCodeGenConfig())
			f.SQLPlan = additiveSQL
			f.SQLPushApplyErr = fmt.Errorf("extension execution failed: Error 1067 (42000): Invalid default value for 'total'")
		},
	})

	if g.exit != 1 {
		t.Errorf("exit = %d, want 1", g.exit)
	}
	if steps := g.er.Steps(); len(steps) != 1 {
		t.Fatalf("expected one confirmation step, got %d", len(steps))
	}
	// The deploy DID happen — the record has to be complete, or the box it just
	// built is unreachable from `destroy`.
	rec := g.onlyDeployment(t)
	if rec.LocalAgentUUID != fakeAgentUUID || rec.ProviderInstanceID != fakeInstanceID || rec.APIURL == "" {
		t.Errorf("record not finalized after a failed schema apply: %+v", rec)
	}
	if rec.LastCompletedStep != deploy.StepFinalized {
		t.Errorf("LastCompletedStep = %q, want %q — the DEPLOYMENT finished, the schema step did not",
			rec.LastCompletedStep, deploy.StepFinalized)
	}
}

// A schema apply that failed BEFORE any SQL was sent: resolving the sql-push
// extension never succeeded, so the database received nothing and there is
// nothing to audit. The distinction from the case above is the whole point.
func TestGoldenSchemaNeverStarted(t *testing.T) {
	g := runDeployGolden(t, "schema_never_started", goldenEnv{
		args:    managedDeployArgs(),
		product: firstDeployAgents,
		er: func(f *fakeExtensionRunner) {
			f.setLastGoCodeGenConfig(savedGoCodeGenConfig())
			f.SQLPlan = additiveSQL
			f.FindExtensionErrs[sqlPushPair.Local] = []error{
				fmt.Errorf("rpc error: code = DeadlineExceeded desc = context deadline exceeded"),
			}
		},
	})

	if g.exit != 1 {
		t.Errorf("exit = %d, want 1", g.exit)
	}
	// The confirmation step was never reached, which is what makes "no SQL was
	// sent" a fact rather than a hope.
	if steps := g.er.Steps(); len(steps) != 0 {
		t.Errorf("a run that never resolved the extension raised %d steps", len(steps))
	}
	rec := g.onlyDeployment(t)
	if rec.LocalAgentUUID != fakeAgentUUID || rec.APIURL == "" {
		t.Errorf("record not finalized: %+v", rec)
	}
	if rec.LastCompletedStep != deploy.StepFinalized {
		t.Errorf("LastCompletedStep = %q, want %q", rec.LastCompletedStep, deploy.StepFinalized)
	}
}

// The custom-application zone carried forward from the project's saved config
// because --custom was not passed. The notice is the fix for a setting that used
// to travel in silence — and the report's "Add custom endpoints" block follows
// the RESOLVED value, not the flag.
//
// Reference: r4-e2e/deploy-my-nocustom.log, which predates the notice.
func TestGoldenCustomStickinessNotice(t *testing.T) {
	g := runDeployGolden(t, "custom_stickiness_notice", goldenEnv{
		args:    managedDeployArgs(),
		product: firstDeployAgents,
		er: func(f *fakeExtensionRunner) {
			saved := savedGoCodeGenConfig()
			saved["custom_enabled"] = true
			f.setLastGoCodeGenConfig(saved)
		},
	})

	if g.exit != 0 {
		t.Errorf("exit = %d, want 0", g.exit)
	}
	// What was announced is what was generated: the saved value reached the
	// generator config rather than being reported and then dropped.
	if got := g.er.SavedConfig()["custom_enabled"]; got != true {
		t.Errorf("custom_enabled in the saved config = %v, want true", got)
	}
	if rec := g.onlyDeployment(t); rec.LastCompletedStep != deploy.StepFinalized {
		t.Errorf("LastCompletedStep = %q, want %q", rec.LastCompletedStep, deploy.StepFinalized)
	}
}

// The bootstrap failed on the box. Everything before it stands: the VM exists,
// the record carries its instance id, and `destroy` can remove it.
func TestGoldenBootstrapFailed(t *testing.T) {
	g := runDeployGolden(t, "bootstrap_failed", goldenEnv{
		args:    managedDeployArgs(),
		product: firstDeployAgents,
		er: func(f *fakeExtensionRunner) {
			f.setLastGoCodeGenConfig(savedGoCodeGenConfig())
		},
		ssh: func(r *fakeRemoteRunner) {
			// What the BOX handed back. RunScript adds the noun, exactly as
			// *SSHRunner does, so the golden below pins the sentence a real
			// failed bootstrap prints.
			r.RunScriptErr = fmt.Errorf("exit status 1")
		},
	})

	if g.exit != 1 {
		t.Errorf("exit = %d, want 1", g.exit)
	}
	// The script that ran was the bootstrap, and it is named as such.
	if labels := g.ssh.ScriptLabels(); len(labels) != 1 || labels[0] != deploy.ScriptBootstrap {
		t.Errorf("RunScript labels = %v, want exactly [%q]", labels, deploy.ScriptBootstrap)
	}
	rec := g.onlyDeployment(t)
	if rec.ProviderInstanceID != fakeInstanceID {
		t.Errorf("ProviderInstanceID = %q — a VM was created and the record cannot address it", rec.ProviderInstanceID)
	}
	if rec.Provisioning {
		t.Error("Provisioning is still set although the box exists; destroy would skip its on-box teardown")
	}
	// No agent and no URLs: the deploy never got that far, and claiming either
	// would make a half-built box look finished.
	if rec.LocalAgentUUID != "" || rec.APIURL != "" {
		t.Errorf("a failed bootstrap recorded pairing/front-door state: agent=%q url=%q", rec.LocalAgentUUID, rec.APIURL)
	}
	// Where it stopped and why, on the record the next run reads. This is the
	// pair the checkpoint exists for: the step says the box was recorded and
	// nothing beyond it happened, the error says what stopped it. Both are
	// written by THIS run about ITS own record — the revision hook only annotates
	// once the box exists and the revision was opened.
	if rec.LastCompletedStep != deploy.StepBoxRecorded {
		t.Errorf("LastCompletedStep = %q, want %q — the box was recorded and the bootstrap then failed",
			rec.LastCompletedStep, deploy.StepBoxRecorded)
	}
	if !strings.Contains(rec.LastError, "remote bootstrap script failed") {
		t.Errorf("LastError = %q, want the bootstrap failure", rec.LastError)
	}
	// The revision in nuzur is marked FAILED rather than left "Deploying…".
	var failed bool
	for _, c := range g.product.CallsTo("UpdateDeploymentRevisionStatus") {
		if c.Params["status"] == "DEPLOYMENT_REVISION_STATUS_FAILED" {
			failed = true
		}
	}
	if !failed {
		t.Error("the deployment revision was not marked FAILED")
	}
}

// ── pre-effect checks (wave 3) ───────────────────────────────────────────────

// The CLI release the box would install does not exist — a dev build, or a
// release whose assets are still uploading.
//
// The bootstrap downloads it in its last section, so before this check the deploy
// failed there: after the VM, Docker, MySQL and the application image had all been
// created and paid for. Now it stops with nothing provisioned, and the refusal
// names the URL it asked about and the flag that skips the check.
func TestGoldenReleaseAsset404(t *testing.T) {
	g := runDeployGolden(t, "release_asset_404", goldenEnv{
		args:    managedDeployArgs(),
		product: firstDeployAgents,
		er: func(f *fakeExtensionRunner) {
			f.setLastGoCodeGenConfig(savedGoCodeGenConfig())
		},
		http: func(rt *fakeRoundTripper) { rt.Status = 404 },
	})

	if g.exit != 1 {
		t.Errorf("exit = %d, want 1", g.exit)
	}
	// The claim of the whole scenario: nothing was created. No VM, no record, no
	// token, no report to nuzur.
	if calls := g.prov.Calls(); len(calls) != 0 {
		t.Errorf("a refused release probe still reached the provider: %v", calls)
	}
	if deps := g.deployments(t); len(deps) != 0 {
		t.Errorf("a refused release probe left a deployment record: %v", deps)
	}
	if calls := g.product.CallsTo("IssueProvisioningToken"); len(calls) != 0 {
		t.Errorf("a provisioning token was issued after the probe refused: %v", calls)
	}
	if calls := g.ssh.Calls(); len(calls) != 0 {
		t.Errorf("a refused release probe reached a box: %v", calls)
	}
	// It asked about the URL the bootstrap would actually fetch — the single-source
	// claim deploy.CLIReleaseAssetURL exists for, checked here end to end rather
	// than only against the template.
	want := "HEAD " + deploy.CLIReleaseAssetURL(constants.CLI_VERSION, deploy.CLIReleaseArchX8664)
	if reqs := g.http.Requests(); len(reqs) != 1 || reqs[0] != want {
		t.Errorf("probe requests = %v, want exactly [%q]", reqs, want)
	}
}

// GitHub could not be reached at all. The probe cannot tell whether the release
// exists, so it says what it does not know and gets out of the way — the same
// one-directional best-effort rule the pre-flight schema gate follows. The deploy
// then runs to completion exactly as first_managed_deploy does.
func TestGoldenReleaseAssetProbeUnreachable(t *testing.T) {
	g := runDeployGolden(t, "release_asset_timeout", goldenEnv{
		args:    managedDeployArgs(),
		product: firstDeployAgents,
		er: func(f *fakeExtensionRunner) {
			f.setLastGoCodeGenConfig(savedGoCodeGenConfig())
		},
		http: func(rt *fakeRoundTripper) {
			rt.Err = context.DeadlineExceeded
		},
	})

	if g.exit != 0 {
		t.Errorf("exit = %d, want 0 — a probe that could not run must not fail a deploy", g.exit)
	}
	rec := g.onlyDeployment(t)
	if rec.LastCompletedStep != deploy.StepFinalized || rec.LocalAgentUUID != fakeAgentUUID {
		t.Errorf("the deploy did not complete after a warned probe: %+v", rec)
	}
}

// A first deploy whose schema cannot be rendered. Without this check the render
// failure surfaces at step 10, on a server that exists, bills, and is running an
// application against a database that was never created.
//
// Note where the transcript stops: before generation, before the token, before the
// VM. The pre-check is placed immediately after the point where the box decision
// is made and before anything is produced.
func TestGoldenFirstDeployUnrenderableSchema(t *testing.T) {
	g := runDeployGolden(t, "first_deploy_unrenderable_schema", goldenEnv{
		args:    managedDeployArgs(),
		product: firstDeployAgents,
		er: func(f *fakeExtensionRunner) {
			f.setLastGoCodeGenConfig(savedGoCodeGenConfig())
			f.CreateSQLErr = fmt.Errorf(
				"extension execution failed: entity \"invoice\" references enum \"currency\", which is not in this project version")
		},
	})

	if g.exit != 1 {
		t.Errorf("exit = %d, want 1", g.exit)
	}
	if calls := g.prov.Calls(); len(calls) != 0 {
		t.Errorf("a blocked first deploy reached the provider: %v", calls)
	}
	if deps := g.deployments(t); len(deps) != 0 {
		t.Errorf("a blocked first deploy left a deployment record: %v", deps)
	}
	if calls := g.product.CallsTo("IssueProvisioningToken"); len(calls) != 0 {
		t.Errorf("a blocked first deploy issued a provisioning token: %v", calls)
	}
	// Nothing was generated either: the check runs BEFORE the generator, so a
	// project that cannot be deployed does not get a workspace written for it.
	if containsCall(g.er.Calls(), "Run "+goCodeGenExtensionIdentifier) {
		t.Errorf("the generator ran despite the schema being unrenderable: %v", g.er.Calls())
	}
}

// A project with no standalone entities. This is not a broken schema — an
// entity-less project deploys perfectly well, it simply has no tables — so the
// check warns and the deploy runs to completion, ending with a schema apply that
// has nothing to do.
func TestGoldenFirstDeployNoEntitiesWarns(t *testing.T) {
	g := runDeployGolden(t, "first_deploy_no_entities_warns", goldenEnv{
		args:    managedDeployArgs(),
		product: firstDeployAgents,
		er: func(f *fakeExtensionRunner) {
			f.setLastGoCodeGenConfig(savedGoCodeGenConfig())
			f.StandaloneEntities = nil
		},
	})

	if g.exit != 0 {
		t.Errorf("exit = %d, want 0 — an entity-less project is deployable", g.exit)
	}
	rec := g.onlyDeployment(t)
	if rec.LastCompletedStep != deploy.StepFinalized {
		t.Errorf("LastCompletedStep = %q, want %q", rec.LastCompletedStep, deploy.StepFinalized)
	}
	// The warning came from the entity lookup and cost nothing else: sql-gen was
	// never run, because there was nothing to hand it.
	if containsCall(g.er.Calls(), "Run "+sqlGenExtensionIdentifier) {
		t.Errorf("sql-gen ran for a project with no entities: %v", g.er.Calls())
	}
}

// The argv guard. `--custom false` sets --custom to TRUE and leaves "false" as a
// positional, at which point flag parsing stops and --allow-destructive is never
// applied — so the run is refused before anything is resolved or provisioned.
//
// Driven through the command's Action, which is where the guard lives. Note the
// error itself surfaces via main's log.Fatal, outside this seam: what the golden
// records is the text and the exit code, without log's timestamp prefix.
func TestGoldenArgvReject(t *testing.T) {
	g := runDeployGolden(t, "argv_reject", goldenEnv{
		args: append(managedDeployArgs(), "--custom", "false", "--allow-destructive"),
	})

	if g.exit != 1 {
		t.Errorf("exit = %d, want 1", g.exit)
	}
	// Refused before login: nothing was resolved, nothing was reached.
	if calls := g.er.Calls(); len(calls) != 0 {
		t.Errorf("the guard let the run reach the extension server: %v", calls)
	}
	if calls := g.product.Calls(); len(calls) != 0 {
		t.Errorf("the guard let the run reach nuzur: %v", calls)
	}
}

// ── destroy ──────────────────────────────────────────────────────────────────

// Tearing down the last project on a box: the on-box cleanup runs, the shared
// agent is revoked, the VM is deleted, and the local record goes.
func TestGoldenDestroyLastOnBox(t *testing.T) {
	g := runDeployGolden(t, "destroy_last_on_box", goldenEnv{
		command: "destroy",
		args:    []string{seedDeploymentID},
		seed: func(work string) []*deploy.Deployment {
			return []*deploy.Deployment{deployedRecord(work)}
		},
	})

	if g.exit != 0 {
		t.Errorf("exit = %d, want 0", g.exit)
	}
	if deps := g.deployments(t); len(deps) != 0 {
		t.Errorf("the record survived destroy: %v", deps)
	}
	if calls := g.product.CallsTo("RevokeLocalAgent"); len(calls) != 1 {
		t.Errorf("RevokeLocalAgent called %d times, want 1 (last project on the box)", len(calls))
	}
	if calls := g.product.CallsTo("MarkDeploymentDestroyed"); len(calls) != 1 {
		t.Errorf("MarkDeploymentDestroyed called %d times, want 1", len(calls))
	}
	if !containsPrefix(g.prov.Calls(), "Destroy ") {
		t.Errorf("the VM was not deleted: %v", g.prov.Calls())
	}
}

// Tearing down one project of two on the same box. The agent survives for the
// other project, and the VM is not touched — a delete here would take the
// sibling with it.
func TestGoldenDestroyWithSibling(t *testing.T) {
	g := runDeployGolden(t, "destroy_with_sibling", goldenEnv{
		command: "destroy",
		args:    []string{seedDeploymentID},
		seed: func(work string) []*deploy.Deployment {
			sibling := deployedRecord(work)
			sibling.ID = siblingDeploymentID
			sibling.Identifier = "billing"
			sibling.ConnUUID = fakeSiblingConnUUID
			sibling.WorkspaceDir = filepath.Join(work, "nuzur-billing")
			return []*deploy.Deployment{deployedRecord(work), sibling}
		},
	})

	if g.exit != 0 {
		t.Errorf("exit = %d, want 0", g.exit)
	}
	deps := g.deployments(t)
	if len(deps) != 1 {
		t.Fatalf("expected the sibling to survive, got %d records: %v", len(deps), deps)
	}
	if _, ok := deps[siblingDeploymentID]; !ok {
		t.Errorf("the surviving record is not the sibling: %v", deps)
	}
	if calls := g.product.CallsTo("RevokeLocalAgent"); len(calls) != 0 {
		t.Errorf("the shared agent was revoked while another project still uses it: %v", calls)
	}
	// The catalog is re-published without this project's connection, and WITH
	// the sibling's — a replace that dropped the sibling would erase it from the
	// data manager.
	calls := g.product.CallsTo("UpdateLocalAgentConnections")
	if len(calls) != 1 {
		t.Fatalf("UpdateLocalAgentConnections called %d times, want 1", len(calls))
	}
	if got := calls[0].Params["connections"]; got != fakeSiblingConnUUID {
		t.Errorf("re-published catalog = %q, want just the sibling's connection %q", got, fakeSiblingConnUUID)
	}
	if calls := g.prov.Calls(); len(calls) != 0 {
		t.Errorf("the VM was touched while another project is still on it: %v", calls)
	}
}

// Destroying a deployment whose VM was deleted out of band. A 404 from the
// delete means the same thing as a 404 from the lookup: nothing to do and
// nothing billing — not a trip to the provider console.
func TestGoldenDestroyVMAlreadyGone(t *testing.T) {
	g := runDeployGolden(t, "destroy_vm_already_gone", goldenEnv{
		command: "destroy",
		args:    []string{seedDeploymentID},
		seed: func(work string) []*deploy.Deployment {
			return []*deploy.Deployment{deployedRecord(work)}
		},
		ssh: func(r *fakeRemoteRunner) {
			// The box went with the droplet, so the teardown script cannot run.
			// This is what `ssh` gives back: the exit status, and the diagnosis it
			// wrote to stderr, which RunScript now carries into the error instead
			// of leaving on the terminal several lines above the warning.
			r.RunScriptErr = fmt.Errorf("exit status 255: ssh: connect to host %s port 22: Operation timed out", fakeProvisionedHost)
		},
		prov: func(p *fakeProvisioner) {
			p.DestroyErr = fmt.Errorf("doctl: 404 The resource you were accessing could not be found")
		},
	})

	if g.exit != 0 {
		t.Errorf("exit = %d, want 0", g.exit)
	}
	// Nothing bootstraps during a destroy: the one script it runs is the
	// teardown, and the warning above must call it that.
	if labels := g.ssh.ScriptLabels(); len(labels) != 1 || labels[0] != deploy.ScriptTeardown {
		t.Errorf("RunScript labels = %v, want exactly [%q]", labels, deploy.ScriptTeardown)
	}
	if strings.Contains(g.transcript, "bootstrap") {
		t.Errorf("a destroy transcript mentions a bootstrap:\n%s", g.transcript)
	}
	// And the cause survives into the warning rather than being replaced by the
	// exit code — the half of the bug that made the message useless.
	if !strings.Contains(g.transcript, "Operation timed out") {
		t.Errorf("the teardown warning dropped ssh's own diagnosis:\n%s", g.transcript)
	}
	if deps := g.deployments(t); len(deps) != 0 {
		t.Errorf("the record survived destroy: %v", deps)
	}
	if !deploy.InstanceAlreadyGone(fmt.Errorf("doctl: 404 The resource you were accessing could not be found")) {
		t.Error("the scripted provider error is no longer recognised as 'already gone'; the scenario is testing nothing")
	}
}

// containsPrefix reports whether any call starts with prefix.
func containsPrefix(calls []string, prefix string) bool {
	for _, c := range calls {
		if strings.HasPrefix(c, prefix) {
			return true
		}
	}
	return false
}

// containsCall reports whether a call was recorded exactly.
func containsCall(calls []string, want string) bool {
	return slices.Contains(calls, want)
}

// ── --plan ───────────────────────────────────────────────────────────────────

// `deploy --plan` against a recorded deployment: the migration is computed by
// REJECTING sql-push's confirmation step, so the extension runs nothing.
func TestGoldenPlanDiff(t *testing.T) {
	g := runDeployGolden(t, "plan_diff", goldenEnv{
		args: []string{"--project", "sfapi", "--version", "v_21", "--deployment", seedDeploymentID, "--plan"},
		seed: func(work string) []*deploy.Deployment {
			return []*deploy.Deployment{deployedRecord(work)}
		},
		er: func(f *fakeExtensionRunner) {
			f.setLastGoCodeGenConfig(savedGoCodeGenConfig())
			f.SQLPlan = destructiveSQL
		},
	})

	if g.exit != 0 {
		t.Errorf("exit = %d, want 0 — a plan applies nothing, so it cannot fail a build", g.exit)
	}
	// A plan writes nothing: not to the record store, not to the box, not to
	// nuzur, and not to the provider.
	rec := g.onlyDeployment(t)
	if !rec.CreatedAt.Equal(seedCreatedAt) || rec.APIURL != "http://"+fakeProvisionedHost+":8443" {
		t.Errorf("--plan modified the deployment record: %+v", rec)
	}
	if raw := g.recordJSON(t, seedDeploymentID); strings.Contains(raw, "last_completed_step") ||
		strings.Contains(raw, "last_error") {
		t.Errorf("--plan wrote a checkpoint:\n%s", raw)
	}
	if calls := g.ssh.Calls(); len(calls) != 0 {
		t.Errorf("--plan reached the box: %v", calls)
	}
	if calls := g.prov.Calls(); len(calls) != 0 {
		t.Errorf("--plan reached the provider: %v", calls)
	}
	if calls := g.product.CallsTo("UpsertDeployment"); len(calls) != 0 {
		t.Errorf("--plan reported a deployment to nuzur: %v", calls)
	}
	if steps := g.er.Steps(); len(steps) != 1 {
		t.Fatalf("expected one confirmation step to reject, got %d", len(steps))
	}
}

// `deploy --plan` with no live database. The honest answer is not "nothing to
// say" — it is the CREATE script a first deploy would run, rendered read-only by
// sql-gen.
func TestGoldenPlanCreateFirstDeploy(t *testing.T) {
	g := runDeployGolden(t, "plan_create_first_deploy", goldenEnv{
		args: managedDeployArgs("--plan"),
		er: func(f *fakeExtensionRunner) {
			f.setLastGoCodeGenConfig(savedGoCodeGenConfig())
			f.StandaloneEntities = []*nemgen.Entity{
				{Uuid: "f8888e33-0000-0000-0000-0000000000e1", Identifier: "customer"},
				{Uuid: "f8888e33-0000-0000-0000-0000000000e2", Identifier: "invoice"},
			}
			f.CreateSQL = "CREATE TABLE `customer` (\n  `uuid` char(36) NOT NULL,\n  PRIMARY KEY (`uuid`)\n);\n" +
				"CREATE TABLE `invoice` (\n  `uuid` char(36) NOT NULL,\n  PRIMARY KEY (`uuid`)\n);\n"
		},
	})

	if g.exit != 0 {
		t.Errorf("exit = %d, want 0", g.exit)
	}
	if deps := g.deployments(t); len(deps) != 0 {
		t.Errorf("--plan created a deployment record: %v", deps)
	}
}

// `deploy --plan --json`: the agent-facing contract. apply_sql verbatim is the
// load-bearing field; destructive and caveats are the decision fields.
func TestGoldenPlanJSON(t *testing.T) {
	g := runDeployGolden(t, "plan_json", goldenEnv{
		args: []string{"--project", "sfapi", "--version", "v_21", "--deployment", seedDeploymentID, "--plan", "--json"},
		seed: func(work string) []*deploy.Deployment {
			return []*deploy.Deployment{deployedRecord(work)}
		},
		er: func(f *fakeExtensionRunner) {
			f.setLastGoCodeGenConfig(savedGoCodeGenConfig())
			f.SQLPlan = destructiveSQL
		},
	})

	if g.exit != 0 {
		t.Errorf("exit = %d, want 0", g.exit)
	}
	// The JSON goes to stdout and nothing else does, or a consumer piping it
	// into a parser gets progress lines in its input.
	for _, line := range strings.Split(g.transcript, "\n") {
		if !strings.HasPrefix(line, "OUT ") {
			continue
		}
		body := strings.TrimPrefix(line, "OUT ")
		if body != "" && !strings.HasPrefix(body, "{") && !strings.HasPrefix(body, " ") && !strings.HasPrefix(body, "}") {
			t.Errorf("non-JSON line on stdout in --json mode: %q", body)
		}
	}
}

// ── the goldens themselves ───────────────────────────────────────────────────

// Every golden is machine-written and machine-compared, so the one failure mode
// worth guarding is a golden that was EDITED. An editor that strips trailing
// whitespace turns the `ERR ` of a blank line into `ERR`, and an editor that
// re-wraps turns a long message into two lines that belong to no stream — both
// of which would otherwise surface as an unreadable diff of ANSI-laden text
// rather than as "this file was hand-edited; regenerate it".
//
// It is also the check that the format itself has not drifted: every line of
// every golden is a tagged output line or the final exit line, and there is
// exactly one of the latter.
func TestGoldenFilesAreWellFormed(t *testing.T) {
	entries, err := os.ReadDir(goldenDir())
	if err != nil {
		t.Fatalf("reading the goldens dir: %v", err)
	}
	seen := 0
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".golden") {
			continue
		}
		seen++
		raw, err := os.ReadFile(filepath.Join(goldenDir(), e.Name()))
		if err != nil {
			t.Fatalf("reading %s: %v", e.Name(), err)
		}
		body := string(raw)
		if !strings.HasSuffix(body, "\n") {
			t.Errorf("%s does not end in a newline", e.Name())
		}
		lines := strings.Split(strings.TrimSuffix(body, "\n"), "\n")
		for i, line := range lines {
			last := i == len(lines)-1
			switch {
			case last && strings.HasPrefix(line, "EXIT "):
			case !last && (strings.HasPrefix(line, "OUT ") || strings.HasPrefix(line, "ERR ")):
			case last:
				t.Errorf("%s: last line is %q, want an `EXIT n` line", e.Name(), line)
			default:
				t.Errorf("%s line %d: %q is neither an `OUT ` nor an `ERR ` line "+
					"(a stripped trailing space on a blank line looks exactly like this) — "+
					"regenerate with `go test ./app -run Golden -update`", e.Name(), i+1, line)
			}
		}
	}
	// A guard against the harness silently comparing nothing, e.g. after a
	// rename of the goldens directory.
	if seen < 15 {
		t.Errorf("found %d goldens, want at least the 15 wave-1 scenarios", seen)
	}
}
