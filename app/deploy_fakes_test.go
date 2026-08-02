package app

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	extensiongen "github.com/nuzur/extension-sdk/idl/gen"
	nemgen "github.com/nuzur/nem/idl/gen"
	"github.com/nuzur/nuzur-cli/deploy"
	"github.com/nuzur/nuzur-cli/extensionrun"
	pb "github.com/nuzur/nuzur-cli/protodeps/gen"
)

// deploy_fakes_test.go holds the three seams the deploy pipeline reaches the
// outside world through, other than the product client (which has its own file):
// the box (SSH), the provider (VM lifecycle), and the extension server.
//
// Two rules run through all of them.
//
// DETERMINISM. Every value a fake invents is fixed — the host, the instance id,
// the region, the front-door URL, the ports — so a golden needs no normalization
// for them. The plan's normalization rules exist for values the PRODUCTION code
// mints (a connection uuid, a deployment short id), not for anything here.
//
// LOUD FAILURE. A method a fake has no script for panics naming itself. A fake
// that silently returns a zero value turns a pipeline change into a passing test
// with a quietly different meaning, which is the exact failure mode the golden
// suite exists to catch.

// The fixed identities the fakes hand out. They are deliberately shaped like the
// real thing (uuids that look like uuids) but recognisable on sight, so a golden
// diff reads as a sentence rather than as hex.
const (
	// Every one of these is valid hexadecimal, so they match the uuid shape the
	// golden normalizer looks for and are protected by its exemption list rather
	// than by accidentally not looking like uuids.
	fakeProjectUUID        = "f8888e33-0000-0000-0000-0000000000a1"
	fakeProjectVersionUUID = "f8888e33-0000-0000-0000-0000000000a2"
	fakeTeamUUID           = "f8888e33-0000-0000-0000-0000000000a3"
	fakeGoCodeGenExtUUID   = "f8888e33-0000-0000-0000-0000000000b1"
	fakeGoCodeGenVerUUID   = "f8888e33-0000-0000-0000-0000000000b2"
	fakeSQLPushExtUUID     = "f8888e33-0000-0000-0000-0000000000b3"
	fakeSQLPushVerUUID     = "f8888e33-0000-0000-0000-0000000000b4"
	fakeSQLGenExtUUID      = "f8888e33-0000-0000-0000-0000000000b5"
	fakeSQLGenVerUUID      = "f8888e33-0000-0000-0000-0000000000b6"

	// fakeSecondAgentUUID is the agent a SECOND box pairs — the --new-vm case,
	// where the fresh VM must not be reported as running the old box's agent.
	fakeSecondAgentUUID = "f8888e33-0000-0000-0000-000000000002"

	// fakeProvisionedHost / fakeInstanceID / fakeRegion are what a managed
	// provider hands back. RFC 5737 reserves 203.0.113.0/24 for documentation, so
	// a golden that leaks it points at nothing and a test that somehow reached
	// the network could not connect to anything real.
	fakeProvisionedHost = "203.0.113.10"
	fakeInstanceID      = "fake-inst-1"
	fakeRegion          = "nyc3"
)

// ── the box ──────────────────────────────────────────────────────────────────

// fakeRemoteRunner is the box a deploy talks to.
//
// ONE instance serves every deploy.RemoteRunner the pipeline asks for, because
// the pipeline asks for more than one: the reused-box reachability probe (step
// 1a) and the deploy's own runner (step 7 onward) are built from separate
// factory calls, and a scenario that scripts "the box does not answer" means it
// of the box, not of one of the two handles on it. Targets are recorded per
// call so a test can still assert which host each was aimed at.
type fakeRemoteRunner struct {
	// PingErrs scripts one error per Ping, in order; the last entry repeats. A
	// nil entry is a successful ping. Empty means every ping succeeds.
	PingErrs []error
	// RunScriptErr fails the bootstrap (or the teardown, on destroy) — the
	// longest and most failure-prone remote step, and the one whose failure has
	// to leave a destroyable record behind.
	//
	// It is what the BOX/ssh handed back (`exit status 1`, an ssh connect
	// failure); RunScript wraps it with the caller's label exactly as *SSHRunner
	// does, so a golden pins the sentence production emits. A fake that returned
	// this raw is how destroy_vm_already_gone.golden came to assert a nicer
	// message than the CLI could produce.
	RunScriptErr  error
	RunCommandErr error
	CopyDirErr    error

	// URL is what `cat /etc/nuzur/<identifier>/url` returns — the front door the
	// bootstrap resolved. Empty derives it from the target host, which is what a
	// real IP-only deploy writes there.
	URL string
	// Ports is what `cat /etc/nuzur/<identifier>/ports` returns, in the
	// bootstrap's KEY=VAL-per-line form.
	Ports string

	// BeforeRunScript, when set, runs before the (fake) bootstrap does. It is an
	// OBSERVATION point rather than a script: the bootstrap is the long step
	// between the record being written for the box (6b) and being finalized (12),
	// so it is where a test can read what the record store looks like MID-RUN —
	// which is the only place several of this pipeline's bugs were ever visible.
	BeforeRunScript func(script string)
	// BeforeCapture is the other end of that window: the front-door readback runs
	// after the agent has paired and before the record is finalized, which is the
	// only observable moment the agent-paired checkpoint is the current one.
	BeforeCapture func(command string)

	sudo    bool
	targets []deploy.Target
	pings   int
	calls   []string
	scripts []string
	labels  []string
}

var _ deploy.RemoteRunner = (*fakeRemoteRunner)(nil)

func newFakeRemoteRunner() *fakeRemoteRunner {
	return &fakeRemoteRunner{Ports: "http=8443\ngrpc=8444\ndb=3306\n"}
}

// factory is the deploy.RemoteRunner seam: every target resolves to this one
// runner, with the target recorded.
func (r *fakeRemoteRunner) factory(t deploy.Target) deploy.RemoteRunner {
	r.targets = append(r.targets, t)
	r.calls = append(r.calls, "NewRunner "+t.Host)
	return r
}

func (r *fakeRemoteRunner) SetSudo(sudo bool) {
	r.sudo = sudo
	r.calls = append(r.calls, fmt.Sprintf("SetSudo %v", sudo))
}

func (r *fakeRemoteRunner) Ping(ctx context.Context) error {
	n := r.pings
	r.pings++
	r.calls = append(r.calls, "Ping")
	if len(r.PingErrs) == 0 {
		return nil
	}
	if n >= len(r.PingErrs) {
		n = len(r.PingErrs) - 1
	}
	return r.PingErrs[n]
}

func (r *fakeRemoteRunner) RunCommand(ctx context.Context, command string) error {
	r.calls = append(r.calls, "RunCommand "+command)
	return r.RunCommandErr
}

func (r *fakeRemoteRunner) RunScript(ctx context.Context, label, script string) error {
	if r.BeforeRunScript != nil {
		r.BeforeRunScript(script)
	}
	// The script itself is kept rather than summarized: it is the bootstrap the
	// box would have run, and a test that wants to assert a rendered parameter
	// reached it (the provisioning token, the DB engine, the image tag) has
	// nowhere else to look.
	r.scripts = append(r.scripts, script)
	r.labels = append(r.labels, label)
	r.calls = append(r.calls, fmt.Sprintf("RunScript %s (%d bytes)", label, len(script)))
	if r.RunScriptErr != nil {
		// Same wrapping *SSHRunner applies. See RunScriptErr's doc.
		return fmt.Errorf("remote %s script failed: %w", label, r.RunScriptErr)
	}
	return nil
}

func (r *fakeRemoteRunner) CopyDir(ctx context.Context, localDir, remotePath string) error {
	r.calls = append(r.calls, "CopyDir -> "+remotePath)
	return r.CopyDirErr
}

func (r *fakeRemoteRunner) Capture(ctx context.Context, command string) (string, error) {
	if r.BeforeCapture != nil {
		r.BeforeCapture(command)
	}
	r.calls = append(r.calls, "Capture "+command)
	switch {
	case strings.Contains(command, "/url"):
		if r.URL != "" {
			return r.URL + "\n", nil
		}
		return "http://" + r.lastHost() + ":8443\n", nil
	case strings.Contains(command, "/ports"):
		return r.Ports, nil
	}
	panic(fmt.Sprintf("fakeRemoteRunner: unscripted Capture %q — the deploy path captures only the "+
		"front-door url and the ports file; script it or assert it never happens", command))
}

// lastHost is the host of the most recently built runner, i.e. the box this
// deploy actually reached.
func (r *fakeRemoteRunner) lastHost() string {
	if len(r.targets) == 0 {
		return fakeProvisionedHost
	}
	return r.targets[len(r.targets)-1].Host
}

// Calls returns every remote interaction in order.
func (r *fakeRemoteRunner) Calls() []string { return r.calls }

// Scripts returns the scripts handed to RunScript, in order.
func (r *fakeRemoteRunner) Scripts() []string { return r.scripts }

// ScriptLabels returns the label each RunScript was called with, in order.
func (r *fakeRemoteRunner) ScriptLabels() []string { return r.labels }

// ── the provider ─────────────────────────────────────────────────────────────

// fakeProvisioner is the managed provider's VM lifecycle.
//
// The one behaviour it must reproduce rather than approximate is
// Spec.OnInstanceCreated: every real adapter fires it the instant the provider
// acknowledges the VM and BEFORE waiting for SSH, and that callback is what
// writes the instance id to disk while the deploy is still in flight. A fake
// that returned a Provisioned without firing it would let a regression in the
// "record the VM before it can be lost" behaviour pass unnoticed — which is the
// whole class of bug the harness exists for.
type fakeProvisioner struct {
	Provider deploy.Provider

	Host       string
	InstanceID string
	Region     string

	// CreateErr fails BEFORE the provider acknowledges anything, so
	// OnInstanceCreated never fires — the create call itself was refused.
	CreateErr error
	// ProvisionErr fails AFTER the acknowledgement, which is the shape of a VM
	// that was created and then never became reachable.
	ProvisionErr error
	FirewallErr  error
	DestroyErr   error
	// FindName is what FindInstanceByName resolves to; "" (the default) is the
	// "no such server" answer the real adapters give.
	FindName string
	FindErr  error

	// BeforeProvision and AfterInstanceCreated bracket the create call, the two
	// other points at which a test can observe the record store mid-run: before
	// anything provider-side has happened (the record exists only as a reservation)
	// and the instant the VM is acknowledged, while the deploy is still waiting for
	// SSH. Neither is scripted output; both are windows.
	BeforeProvision      func()
	AfterInstanceCreated func()

	calls []string
}

var _ deploy.Provisioner = (*fakeProvisioner)(nil)

func newFakeProvisioner(p deploy.Provider) *fakeProvisioner {
	return &fakeProvisioner{
		Provider:   p,
		Host:       fakeProvisionedHost,
		InstanceID: fakeInstanceID,
		Region:     fakeRegion,
	}
}

func (p *fakeProvisioner) Provision(ctx context.Context, spec deploy.Spec) (deploy.Provisioned, error) {
	p.calls = append(p.calls, "Provision "+spec.ResourceName)
	if p.BeforeProvision != nil {
		p.BeforeProvision()
	}
	if p.CreateErr != nil {
		return deploy.Provisioned{}, p.CreateErr
	}
	// Fired here, before the (simulated) wait for SSH, exactly as the adapters
	// do — see deploy.reportInstance.
	if spec.OnInstanceCreated != nil {
		spec.OnInstanceCreated(deploy.InstanceRef{
			InstanceID:   p.InstanceID,
			Region:       p.Region,
			Host:         p.Host,
			ResourceName: spec.ResourceName,
		})
	}
	if p.AfterInstanceCreated != nil {
		p.AfterInstanceCreated()
	}
	if p.ProvisionErr != nil {
		return deploy.Provisioned{}, p.ProvisionErr
	}
	return deploy.Provisioned{
		Target: deploy.Target{
			Host:    p.Host,
			User:    spec.Target.User,
			Port:    spec.Target.Port,
			KeyPath: spec.Target.KeyPath,
		},
		InstanceID: p.InstanceID,
		Region:     p.Region,
	}, nil
}

func (p *fakeProvisioner) ConfigureFirewall(ctx context.Context, prov deploy.Provisioned, rules []deploy.FirewallRule) error {
	ports := make([]string, 0, len(rules))
	for _, r := range rules {
		if r.PortEnd != 0 {
			ports = append(ports, fmt.Sprintf("%d-%d", r.Port, r.PortEnd))
			continue
		}
		ports = append(ports, fmt.Sprint(r.Port))
	}
	p.calls = append(p.calls, "ConfigureFirewall "+strings.Join(ports, ","))
	return p.FirewallErr
}

func (p *fakeProvisioner) Destroy(ctx context.Context, prov deploy.Provisioned) error {
	p.calls = append(p.calls, "Destroy "+prov.InstanceID)
	return p.DestroyErr
}

func (p *fakeProvisioner) FindInstanceByName(ctx context.Context, name, region string) (string, error) {
	p.calls = append(p.calls, "FindInstanceByName "+name)
	return p.FindName, p.FindErr
}

// Calls returns every provider interaction in order.
func (p *fakeProvisioner) Calls() []string { return p.calls }

// ── the extension server ─────────────────────────────────────────────────────

// fakeExtensionRunner implements the deploy-path slice of extensionRunner.
//
// The interface has 18 methods; a deploy uses eleven of them, and the rest panic
// naming themselves. What it has to get right is not the discovery calls (canned
// protos) but Run, which is two completely different conversations depending on
// which extension it is handed:
//
//   - go-code-gen writes a workspace. findSourceRoot then looks for a Dockerfile
//     under it, so the fake writes one — without it the deploy fails at step 2
//     with "no Dockerfile found in generated output" and no scenario gets past
//     generation.
//   - sql-push raises a CONFIRMATION step carrying the migration, and the CLI's
//     answer to it is the entire difference between applying a schema and
//     planning one. A rejected step ends the execution CANCELLED, which the real
//     client returns as a POPULATED result alongside ErrExecutionCancelled (see
//     extensionrun/run.go's CANCELLED arm) — the contract --plan and the
//     pre-flight gate both depend on, so the fake reproduces it exactly.
type fakeExtensionRunner struct {
	Project        *nemgen.Project
	ProjectVersion *nemgen.ProjectVersion
	ConfigEntity   *extensiongen.ExtensionConfigurationEntity
	LastConfigs    map[string]extensionrun.LastUsedEntry
	Role           nemgen.UserProjectRole

	// FindExtensionErrs scripts a SEQUENCE of results per extension identifier —
	// one entry per call, the last repeating, a nil entry meaning success.
	//
	// A sequence rather than a single error because a re-deploy resolves
	// "sql-push-local" TWICE: once for the pre-flight gate and once for the
	// apply. The live failure this models (r6/deploy2_reuse.log) hit only the
	// second, so the deploy generated, shipped and bootstrapped before the
	// schema step failed — and scripting a flat error would have failed the
	// pre-flight instead, which is a different (and much quieter) run.
	FindExtensionErrs map[string][]error

	// SQLPlan is the migration sql-push presents at its confirmation step.
	// Empty means the extension short-circuits with "no changes to apply" and
	// raises no step at all — the arm on which nothing is ever decided.
	SQLPlan string
	// SQLPushStatusMessage is the extension's terminal message.
	SQLPushStatusMessage string
	// SQLPushApplyErr fails the run AFTER the step was confirmed, which is the
	// only shape in which a statement can have reached the database.
	SQLPushApplyErr error
	// CreateSQL is what sql-gen renders for a first-deploy create plan.
	CreateSQL string
	// CreateSQLErr fails the sql-gen run — a schema the generator cannot render.
	// It is the failure the first-deploy pre-check exists to catch, and the same
	// one that would otherwise surface at step 10, on a box that already bills.
	CreateSQLErr error

	// StandaloneEntities and StandaloneErr drive computeCreatePlan.
	StandaloneEntities []*nemgen.Entity
	StandaloneErr      error

	GenerateErr     error
	SaveConfigErr   error
	JWTConfigErr    *extensionrun.ConfigValidationError
	JWTWarnings     []string
	JWTCheckErr     error
	BuildConfigErr  error
	ListProjectsErr error

	calls       []string
	findCalls   map[string]int
	savedConfig map[string]interface{}
	steps       []extensionrun.StepPrompt
}

var _ extensionRunner = (*fakeExtensionRunner)(nil)

// newFakeExtensionRunner returns a runner wired for the ordinary deploy: one
// project the user administers, one APPROVED version, the go-code-gen config
// entity, and an empty (no-changes) migration.
func newFakeExtensionRunner() *fakeExtensionRunner {
	return &fakeExtensionRunner{
		Project: &nemgen.Project{
			Uuid:     fakeProjectUUID,
			Name:     "sfapi",
			TeamUuid: fakeTeamUUID,
		},
		ProjectVersion: &nemgen.ProjectVersion{
			Uuid:         fakeProjectVersionUUID,
			Identifier:   "v_21",
			ProjectUuid:  fakeProjectUUID,
			ReviewStatus: nemgen.ProjectVersionReviewStatus_PROJECT_VERSION_REVIEW_STATUS_APPROVED,
		},
		ConfigEntity: goCodeGenConfigEntity(),
		LastConfigs:  map[string]extensionrun.LastUsedEntry{},
		Role:         nemgen.UserProjectRole_USER_PROJECT_ROLE_ADMIN,
		// A project with a schema, by default. Every FIRST deploy renders the
		// CREATE script as a pre-check (checkCreatePlanRenders), so the default
		// project has to be one whose schema renders — a fake with no entities
		// would put the "no standalone entities" warning into every first-deploy
		// golden and would be testing an edge case by accident. The scenarios that
		// want the empty or the broken project say so.
		StandaloneEntities:   defaultStandaloneEntities(),
		CreateSQL:            defaultCreateSQL,
		FindExtensionErrs:    map[string][]error{},
		findCalls:            map[string]int{},
		SQLPushStatusMessage: "No changes to apply",
	}
}

// defaultStandaloneEntities is the two-table schema the fake project has.
func defaultStandaloneEntities() []*nemgen.Entity {
	return []*nemgen.Entity{
		{Uuid: "f8888e33-0000-0000-0000-0000000000e1", Identifier: "customer"},
		{Uuid: "f8888e33-0000-0000-0000-0000000000e2", Identifier: "invoice"},
	}
}

// defaultCreateSQL is what sql-gen renders for it. It reaches a transcript only
// through `deploy --plan`; the first-deploy pre-check discards the script and
// keeps only whether it rendered.
const defaultCreateSQL = "CREATE TABLE `customer` (\n  `uuid` char(36) NOT NULL,\n  PRIMARY KEY (`uuid`)\n);\n" +
	"CREATE TABLE `invoice` (\n  `uuid` char(36) NOT NULL,\n  PRIMARY KEY (`uuid`)\n);\n"

// setLastGoCodeGenConfig installs the project's saved go-code-gen config — the
// difference between a project that has run the generator before (no derived
// defaults, and a --custom that carries itself forward) and one that has not.
func (f *fakeExtensionRunner) setLastGoCodeGenConfig(values map[string]interface{}) {
	f.LastConfigs[goCodeGenExtensionIdentifier] = extensionrun.LastUsedEntry{
		ConfigValues: values,
		LastUsed:     time.Date(2026, 7, 1, 12, 0, 0, 0, time.UTC),
	}
}

// goCodeGenConfigEntity is the generator's config schema, reduced to the fields
// the deploy path reads or fills.
//
// The `required` marks are what applyCodegenDefaults keys on: it fills a
// REQUIRED field nothing else supplies and announces what it filled, so which
// fields are required here decides whether a scenario prints the first-deploy
// "deploying with derived defaults" notice.
func goCodeGenConfigEntity() *extensiongen.ExtensionConfigurationEntity {
	str := extensiongen.ExtensionInputType_EXTENSION_INPUT_TYPE_STRING
	boolean := extensiongen.ExtensionInputType_EXTENSION_INPUT_TYPE_BOOLEAN
	field := func(id string, typ extensiongen.ExtensionInputType, required bool) *extensiongen.ExtensionInputField {
		return &extensiongen.ExtensionInputField{Identifier: id, Type: typ, Required: required}
	}
	return &extensiongen.ExtensionConfigurationEntity{
		Version: "1",
		Fields: []*extensiongen.ExtensionInputField{
			field("identifier", str, true),
			field("go_module", str, true),
			field("db", str, true),
			field("events", str, true),
			field("auth", str, true),
			field("proto_enabled", boolean, true),
			field("grpc_server_enabled", boolean, true),
			field("rest_enabled", boolean, true),
			field("grpc_port", str, true),
			field("http_port", str, true),
			field("dockerfile", boolean, true),
			field("helm", boolean, false),
			field("github_actions", boolean, false),
			field("custom_enabled", boolean, false),
			field("storage_enabled", boolean, false),
			field("object_store", str, false),
			field("rest_base_path", str, false),
		},
	}
}

func (f *fakeExtensionRunner) record(method string, args ...string) {
	if len(args) > 0 {
		f.calls = append(f.calls, method+" "+strings.Join(args, " "))
		return
	}
	f.calls = append(f.calls, method)
}

// Calls returns every extension-server interaction in order.
func (f *fakeExtensionRunner) Calls() []string { return f.calls }

// Steps returns the confirmation steps this run raised, in order.
func (f *fakeExtensionRunner) Steps() []extensionrun.StepPrompt { return f.steps }

// SavedConfig returns the config SaveLastUsedConfigEntry was handed, or nil.
func (f *fakeExtensionRunner) SavedConfig() map[string]interface{} { return f.savedConfig }

func (f *fakeExtensionRunner) panicUnscripted(method string) {
	panic("fakeExtensionRunner: " + method + " was called but is not on the deploy path; " +
		"either the pipeline changed or the fake needs a script for it")
}

// --- discovery ---------------------------------------------------------------

func (f *fakeExtensionRunner) ListUserProjects() ([]*nemgen.Project, error) {
	f.record("ListUserProjects")
	if f.ListProjectsErr != nil {
		return nil, f.ListProjectsErr
	}
	return []*nemgen.Project{f.Project}, nil
}

func (f *fakeExtensionRunner) ListProjectVersions(projectUUID string) ([]*nemgen.ProjectVersion, error) {
	f.record("ListProjectVersions", projectUUID)
	return []*nemgen.ProjectVersion{f.ProjectVersion}, nil
}

func (f *fakeExtensionRunner) FindExtensionByIdentifier(identifier string) (*nemgen.Extension, error) {
	f.record("FindExtensionByIdentifier", identifier)
	if seq, ok := f.FindExtensionErrs[identifier]; ok && len(seq) > 0 {
		n := f.findCalls[identifier]
		f.findCalls[identifier] = n + 1
		if n >= len(seq) {
			n = len(seq) - 1
		}
		if err := seq[n]; err != nil {
			return nil, err
		}
	}
	switch identifier {
	case goCodeGenExtensionIdentifier:
		return &nemgen.Extension{Uuid: fakeGoCodeGenExtUUID, Identifier: identifier}, nil
	case sqlPushPair.Front, sqlPushPair.Local:
		return &nemgen.Extension{Uuid: fakeSQLPushExtUUID, Identifier: identifier}, nil
	case sqlGenExtensionIdentifier:
		return &nemgen.Extension{Uuid: fakeSQLGenExtUUID, Identifier: identifier}, nil
	}
	panic("fakeExtensionRunner: FindExtensionByIdentifier(" + identifier + ") is not a deploy-path extension")
}

func (f *fakeExtensionRunner) GetLatestExtensionVersion(extensionUUID string) (*nemgen.ExtensionVersion, error) {
	f.record("GetLatestExtensionVersion", extensionUUID)
	switch extensionUUID {
	case fakeGoCodeGenExtUUID:
		return &nemgen.ExtensionVersion{Uuid: fakeGoCodeGenVerUUID, ExtensionUuid: extensionUUID}, nil
	case fakeSQLPushExtUUID:
		return &nemgen.ExtensionVersion{Uuid: fakeSQLPushVerUUID, ExtensionUuid: extensionUUID}, nil
	case fakeSQLGenExtUUID:
		return &nemgen.ExtensionVersion{Uuid: fakeSQLGenVerUUID, ExtensionUuid: extensionUUID}, nil
	}
	panic("fakeExtensionRunner: GetLatestExtensionVersion for unknown extension " + extensionUUID)
}

func (f *fakeExtensionRunner) GetConfigEntity(extensionVersion *nemgen.ExtensionVersion) (*extensiongen.ExtensionConfigurationEntity, error) {
	f.record("GetConfigEntity", extensionVersion.GetUuid())
	if extensionVersion.GetUuid() == fakeGoCodeGenVerUUID {
		return f.ConfigEntity, nil
	}
	// sql-push/sql-gen configs are assembled by the CLI (planTarget.configValues,
	// computeCreatePlan), never validated against a schema on this path.
	return &extensiongen.ExtensionConfigurationEntity{}, nil
}

// --- access ------------------------------------------------------------------

func (f *fakeExtensionRunner) GetUserRoleForProject(projectUUID string) (nemgen.UserProjectRole, error) {
	f.record("GetUserRoleForProject", projectUUID)
	return f.Role, nil
}

func (f *fakeExtensionRunner) CheckExtensionExecutionLimit(projectUUID string, extensionUUID string) (*pb.CheckExtensionExecutionLimitResponse, error) {
	f.panicUnscripted("CheckExtensionExecutionLimit (the client-side limit gate was replaced by a server-side queue)")
	return nil, nil
}

// --- config ------------------------------------------------------------------

func (f *fakeExtensionRunner) GetLastUsedConfigs(projectVersionUUID string) (map[string]extensionrun.LastUsedEntry, error) {
	f.record("GetLastUsedConfigs", projectVersionUUID)
	return f.LastConfigs, nil
}

func (f *fakeExtensionRunner) SaveLastUsedConfigEntry(projectVersionUUID, extensionIdentifier string, configValues map[string]interface{}) error {
	f.record("SaveLastUsedConfigEntry", extensionIdentifier)
	if f.SaveConfigErr != nil {
		return f.SaveConfigErr
	}
	f.savedConfig = configValues
	return nil
}

func (f *fakeExtensionRunner) NewConfigResolver(project *nemgen.Project, projectVersionUUID string) *extensionrun.ConfigResolver {
	f.panicUnscripted("NewConfigResolver (interactive config building is not on the deploy path)")
	return nil
}

func (f *fakeExtensionRunner) DescribeConfig(
	project *nemgen.Project,
	projectVersion *nemgen.ProjectVersion,
	extension *nemgen.Extension,
	extensionVersion *nemgen.ExtensionVersion,
	configEntity *extensiongen.ExtensionConfigurationEntity,
	lastConfig map[string]interface{},
) (*extensionrun.ConfigSchema, error) {
	f.panicUnscripted("DescribeConfig (the `describe` subcommand, not deploy)")
	return nil, nil
}

// BuildConfigFromJSON reproduces the real merge: `provided` over `lastConfig`,
// restricted to the fields the extension declares, with the same coercion
// (booleans stay bool, every other scalar becomes a string) and the same
// all-at-once required-field error.
//
// The coercion is not cosmetic. runDeploy reads the RESULT — boolValue for
// rest_enabled/grpc_server_enabled/custom_enabled, stringValue for auth and
// rest_base_path — so a fake that handed back the raw map would make the
// deploy's own report describe a different app from the one the generator was
// asked for.
func (f *fakeExtensionRunner) BuildConfigFromJSON(
	project *nemgen.Project,
	projectVersionUUID string,
	configEntity *extensiongen.ExtensionConfigurationEntity,
	provided map[string]interface{},
	lastConfig map[string]interface{},
) (map[string]interface{}, error) {
	f.record("BuildConfigFromJSON")
	if f.BuildConfigErr != nil {
		return nil, f.BuildConfigErr
	}
	values := map[string]interface{}{}
	if configEntity == nil || len(configEntity.Fields) == 0 {
		return values, nil
	}
	var fieldErrs []extensionrun.FieldError
	known := map[string]bool{}
	for _, field := range configEntity.Fields {
		known[field.Identifier] = true
	}
	for key := range provided {
		if !known[key] {
			fieldErrs = append(fieldErrs, extensionrun.FieldError{
				Field: key, Message: "unknown config field for this extension",
			})
		}
	}
	for _, field := range configEntity.Fields {
		raw, ok := provided[field.Identifier]
		if !ok || raw == nil {
			raw, ok = lastConfig[field.Identifier]
		}
		if !ok || raw == nil {
			if field.Required {
				fieldErrs = append(fieldErrs, extensionrun.FieldError{
					Field: field.Identifier, Message: "required field is missing",
				})
			}
			continue
		}
		if field.Type == extensiongen.ExtensionInputType_EXTENSION_INPUT_TYPE_BOOLEAN {
			b, err := coerceFakeBool(raw)
			if err != nil {
				fieldErrs = append(fieldErrs, extensionrun.FieldError{Field: field.Identifier, Message: err.Error()})
				continue
			}
			values[field.Identifier] = b
			continue
		}
		values[field.Identifier] = fmt.Sprintf("%v", raw)
	}
	if len(fieldErrs) > 0 {
		return nil, &extensionrun.ConfigValidationError{Fields: fieldErrs}
	}
	return values, nil
}

func coerceFakeBool(raw interface{}) (bool, error) {
	switch t := raw.(type) {
	case bool:
		return t, nil
	case string:
		switch strings.ToLower(strings.TrimSpace(t)) {
		case "true":
			return true, nil
		case "false":
			return false, nil
		}
	}
	return false, fmt.Errorf("expected a boolean, got %v", raw)
}

// --- schema facts ------------------------------------------------------------

func (f *fakeExtensionRunner) GetStandaloneEntities(projectVersionUUID string) ([]*nemgen.Entity, error) {
	f.record("GetStandaloneEntities", projectVersionUUID)
	return f.StandaloneEntities, f.StandaloneErr
}

func (f *fakeExtensionRunner) ValidateJWTAuthRequirements(projectVersionUUID string, configValues map[string]interface{}) (*extensionrun.ConfigValidationError, []string, error) {
	f.record("ValidateJWTAuthRequirements", projectVersionUUID)
	return f.JWTConfigErr, f.JWTWarnings, f.JWTCheckErr
}

// --- the execution itself ----------------------------------------------------

func (f *fakeExtensionRunner) Run(params extensionrun.RunParams) (*extensionrun.RunResult, error) {
	identifier := params.Extension.GetIdentifier()
	f.record("Run", identifier)
	switch identifier {
	case goCodeGenExtensionIdentifier:
		return f.runGenerator(params)
	case sqlPushPair.Front, sqlPushPair.Local:
		return f.runSQLPush(params)
	case sqlGenExtensionIdentifier:
		if f.CreateSQLErr != nil {
			return nil, f.CreateSQLErr
		}
		return &extensionrun.RunResult{
			Status:        "succeeded",
			ExecutionUUID: "fake-exec-sql-gen",
			OutputPath:    params.OutputPath,
			DisplayBlocks: []extensionrun.DisplayBlock{{
				Identifier:  "create",
				ContentType: "sql",
				Content:     f.CreateSQL,
			}},
		}, nil
	}
	panic("fakeExtensionRunner: Run(" + identifier + ") is not a deploy-path extension")
}

// runGenerator writes the smallest workspace the deploy can proceed from.
//
// The generator nests the app under a directory named for the config's
// identifier, and findSourceRoot locates the app by looking for a Dockerfile —
// so the Dockerfile is the load-bearing file, and the nesting is what makes
// sourceRoot differ from workspaceDir the way it does in production (the deploy
// reports the app dir as "Your app source" and copies THAT to the box, while the
// record keeps the workspace root).
//
// config/base.yaml is written with a jwt block when the resolved config asked
// for JWT auth, because generatedHasJWTAuth reads exactly that file to decide
// whether the box needs a signing key and whether the report mentions /signin.
func (f *fakeExtensionRunner) runGenerator(params extensionrun.RunParams) (*extensionrun.RunResult, error) {
	if f.GenerateErr != nil {
		return nil, f.GenerateErr
	}
	identifier := stringValue(params.ConfigValues, "identifier", "app")
	appDir := filepath.Join(params.OutputPath, identifier)
	if err := os.MkdirAll(filepath.Join(appDir, "config"), 0o755); err != nil {
		return nil, err
	}
	base := "service:\n  name: " + identifier + "\n"
	if stringValue(params.ConfigValues, "auth", "") == "jwt" {
		base += "auth:\n  jwt:\n    issuer: nuzur\n"
	}
	// Written in a fixed order: FilesWritten is part of the RunResult, and a map
	// range would make it differ between runs.
	toWrite := []struct{ path, content string }{
		{filepath.Join(appDir, "Dockerfile"), "FROM golang:1.24 AS build\n"},
		{filepath.Join(appDir, "go.mod"), "module " + stringValue(params.ConfigValues, "go_module", identifier) + "\n\ngo 1.24\n"},
		{filepath.Join(appDir, "config", "base.yaml"), base},
	}
	written := make([]string, 0, len(toWrite))
	for _, w := range toWrite {
		if err := os.WriteFile(w.path, []byte(w.content), 0o644); err != nil {
			return nil, err
		}
		written = append(written, w.path)
	}
	return &extensionrun.RunResult{
		Status:        "succeeded",
		ExecutionUUID: "fake-exec-go-code-gen",
		OutputPath:    params.OutputPath,
		FilesWritten:  written,
	}, nil
}

// runSQLPush is the confirmation-step conversation.
//
// The three outcomes it can produce are the three the deploy has to tell apart:
// a run with nothing to do (no step is ever raised), a run whose step was
// CONFIRMED (statements were issued, so a later failure can have landed half a
// migration), and a run whose step was REJECTED — by --plan on purpose, or by
// the destructive gate — which ends CANCELLED with the plan still in hand.
func (f *fakeExtensionRunner) runSQLPush(params extensionrun.RunParams) (*extensionrun.RunResult, error) {
	if f.SQLPlan == "" {
		// The extension's own short-circuit: no diff, no step, terminal success.
		return &extensionrun.RunResult{
			Status:        "succeeded",
			ExecutionUUID: "fake-exec-sql-push",
			OutputPath:    params.OutputPath,
			StatusMessage: f.SQLPushStatusMessage,
		}, nil
	}
	prompt := extensionrun.StepPrompt{
		StepIdentifier:  "sql-validation",
		BlockIdentifier: "sql-diff",
		Title:           "Review the changes",
		ContentType:     "sql",
		Content:         f.SQLPlan,
	}
	f.steps = append(f.steps, prompt)

	decide := params.OnConfirmationStep
	if decide == nil {
		// Verbatim the non-interactive fallback in RunParams.stepDecider: a
		// confirmation step with nobody to answer it is an error, not a yes.
		return nil, fmt.Errorf("extension is waiting on confirmation step %q and this run is non-interactive; enable step auto-confirmation to proceed", prompt.StepIdentifier)
	}
	decision, err := decide(prompt)
	if err != nil {
		// A decider that errors aborts WITHOUT answering the step — the run
		// returns no result at all.
		return nil, err
	}
	outcome := extensionrun.StepOutcome{Prompt: prompt, Confirmed: decision.Confirm, Reason: decision.Reason}
	if !decision.Confirm {
		// The contract from extensionrun/run.go's CANCELLED arm: a populated
		// result ALONGSIDE the sentinel, so the caller that rejected on purpose
		// still has the plan it rejected.
		//
		// StatusMessage is the extension SERVER's own terminal text, which no
		// captured transcript records for a cancelled run — so this value is
		// plausible rather than verbatim. It surfaces in one place only: the
		// `message` field of `deploy --plan --json`. Everything else about this
		// arm (the populated result, the sentinel, the retained step) is the
		// real contract.
		return &extensionrun.RunResult{
			Status:        "cancelled",
			ExecutionUUID: "fake-exec-sql-push",
			OutputPath:    params.OutputPath,
			StatusMessage: "execution cancelled by the client",
			Steps:         []extensionrun.StepOutcome{outcome},
		}, extensionrun.ErrExecutionCancelled
	}
	if f.SQLPushApplyErr != nil {
		return nil, f.SQLPushApplyErr
	}
	return &extensionrun.RunResult{
		Status:        "succeeded",
		ExecutionUUID: "fake-exec-sql-push",
		OutputPath:    params.OutputPath,
		StatusMessage: f.SQLPushStatusMessage,
		Steps:         []extensionrun.StepOutcome{outcome},
	}, nil
}

// ── GitHub (the release-asset probe) ─────────────────────────────────────────

// fakeRoundTripper answers the one outbound HTTP request the CLI makes: the
// HEAD on the nuzur-cli release asset the box would install
// (deploy_release_probe.go).
//
// It defaults to 200, which is what a released CLI version gets and what every
// pre-existing scenario needs — the probe is silent on success, so a golden
// authored before it existed stays valid. A scenario that wants the missing
// release, or an unreachable GitHub, scripts it.
//
// Not scripting it at all is the dangerous case: a nil transport is the
// PRODUCTION one, so a test that forgot this would reach github.com for real, and
// would then pass or fail depending on the network. runDeployGolden therefore
// always installs one.
type fakeRoundTripper struct {
	// Status is the response code; 0 means 200.
	Status int
	// Err fails the round trip itself — a timeout, DNS, a proxy refusing.
	Err error

	requests []string
}

var _ http.RoundTripper = (*fakeRoundTripper)(nil)

func newFakeRoundTripper() *fakeRoundTripper { return &fakeRoundTripper{} }

func (f *fakeRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	f.requests = append(f.requests, req.Method+" "+req.URL.String())
	if f.Err != nil {
		// net/http wraps a transport error in *url.Error, which is what the probe
		// sees from client.Do — reproduced so the warning text is the real one.
		return nil, f.Err
	}
	status := f.Status
	if status == 0 {
		status = http.StatusOK
	}
	return &http.Response{
		StatusCode: status,
		Status:     fmt.Sprintf("%d %s", status, http.StatusText(status)),
		Proto:      "HTTP/1.1",
		Header:     http.Header{},
		Body:       io.NopCloser(strings.NewReader("")),
		Request:    req,
	}, nil
}

// Requests returns every outbound request, in order.
func (f *fakeRoundTripper) Requests() []string { return f.requests }

// --- unscripted ---------------------------------------------------------------

func (f *fakeExtensionRunner) ListGeneratorExtensions() ([]*nemgen.Extension, error) {
	f.panicUnscripted("ListGeneratorExtensions (interactive extension picking, not deploy)")
	return nil, nil
}

func (f *fakeExtensionRunner) ListRunnableExtensions(pairFronts []string) ([]*nemgen.Extension, error) {
	f.panicUnscripted("ListRunnableExtensions (interactive extension picking, not deploy)")
	return nil, nil
}
