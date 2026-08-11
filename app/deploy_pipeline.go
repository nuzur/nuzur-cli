package app

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/gofrs/uuid"
	nemgen "github.com/nuzur/nem/idl/gen"
	"github.com/nuzur/nuzur-cli/constants"
	"github.com/nuzur/nuzur-cli/deploy"
	"github.com/nuzur/nuzur-cli/extensionrun"
	"github.com/nuzur/nuzur-cli/outputtools"
	"github.com/nuzur/nuzur-cli/productclient"
	pb "github.com/nuzur/nuzur-cli/protodeps/gen"
	"github.com/nuzur/nuzur-cli/sqlplan"
)

// deploy_pipeline.go is the deploy: its STATE, its STEPS and the LIST that
// orders them. command_deploy.go keeps the entry point, the interrupt handling
// and the helpers the steps call.
//
// The state exists because a deploy carries roughly forty values from one end of
// the pipeline to the other — the resolved settings, the names derived from them,
// which box was chosen, what the generator produced, and everything the run
// learns as it goes. As locals in one long function those were invisible: nothing
// said which step established a value and which merely read it, and a step could
// not be moved, tested or skipped without dragging the whole body with it.
//
// Grouped, not flat: the groups below are the deploy's actual phases, and the
// order of the fields is the order in which they are filled in.
//
// The list exists because the properties that kept breaking are properties OF THE
// ORDER, not of any step: that checks run before effects, that anything with a
// consequence leaves a recoverable trace, that the checkpoints go forwards. As a
// straight-line function those could only be re-established by reading 1100 lines
// and hoping. As data they are three unit tests (deploy_pipeline_test.go), and
// they run on every commit.

// remoteSrcDir is where the generated source is copied on the box. scp runs as
// the SSH user (which may be non-root); the sudo bootstrap builds from here.
const remoteSrcDir = "/tmp/nuzur-src"

// ── effects ──────────────────────────────────────────────────────────────────

// effectLevel is a TIER a step can leave something behind in, ordered by how
// hard that something is to take back. Terminal output, reads and pure
// computation are not effects: what is classified here is what survives the
// process.
//
// The ordering is the point. "Has this deploy started doing things that outlive
// it, and how far up?" is the question every recovery decision turns on, and it
// used to be answerable only by reading the function.
type effectLevel uint8

const (
	// effNone is the zero of the ordering, not a tier: it is what Max reports
	// for a step that leaves nothing behind.
	effNone effectLevel = iota
	// effLocalFS: files on THIS machine outside the record store — the generated
	// workspace. Recoverable by hand, and the user's own edits live there.
	effLocalFS
	// effRecord: the local deployment record store, which is what `destroy` and
	// the next deploy read.
	effRecord
	// effCloud: state in nuzur — deployment rows, revisions, the connection
	// catalog, a provisioning token.
	effCloud
	// effBox: the target server itself — packages, files, containers, the
	// database and its data.
	effBox
	// effProvider: resources the provider BILLS for. The tier that made this
	// classification worth having.
	effProvider
)

func (l effectLevel) String() string {
	switch l {
	case effLocalFS:
		return "localFS"
	case effRecord:
		return "record"
	case effCloud:
		return "cloud"
	case effBox:
		return "box"
	case effProvider:
		return "provider"
	}
	return "none"
}

// effectSet is the set of tiers one step touches, as a bitfield.
type effectSet uint8

// effects builds a set. effects() — no arguments — is the empty set, which is
// how a pure step declares itself.
func effects(levels ...effectLevel) effectSet {
	var s effectSet
	for _, l := range levels {
		s |= 1 << l
	}
	return s
}

func (s effectSet) Has(l effectLevel) bool { return s&(1<<l) != 0 }

// Max is the highest tier in the set, effNone for the empty set.
func (s effectSet) Max() effectLevel {
	for l := effProvider; l > effNone; l-- {
		if s.Has(l) {
			return l
		}
	}
	return effNone
}

func (s effectSet) String() string {
	var parts []string
	for l := effLocalFS; l <= effProvider; l++ {
		if s.Has(l) {
			parts = append(parts, l.String())
		}
	}
	if len(parts) == 0 {
		return "none"
	}
	return strings.Join(parts, "|")
}

// ── the step ─────────────────────────────────────────────────────────────────

// deployStep is one step of the deploy, described well enough that the list can
// be asserted on without running anything.
type deployStep struct {
	// name is what this step is called in a test failure — and, from wave 5, in
	// what the user is told about an interrupted run.
	name string

	// effects is what running this step can leave behind. Declared, and pinned
	// by the structural tests: an effect nobody declared is how "the check runs
	// after the money is spent" survives review.
	effects effectSet

	// checkpoint is the deploy.Step* this step ESTABLISHES: once it has
	// succeeded, that is what the record says about how far the deploy got.
	//
	// Declared here, WRITTEN inside the step — deliberately, and this is the one
	// place the list does not centralize something it could. A checkpoint is a
	// claim about the record, and it is only true if it lands in the same write
	// as the facts it describes: the pending record IS the resource-name
	// reservation, StepInstanceCreated is written from inside the provider's
	// acknowledgement callback (minutes before the step returns), and the pairing
	// checkpoint rides the write that stores the agent uuid. A loop that wrote
	// checkpoints after the fact would re-introduce, at every step, exactly the
	// window this field exists to close — a record that has the fact but not the
	// checkpoint, or the checkpoint but not the fact. The steps' own failure
	// policies differ too (fatal for the record writes, best-effort for the
	// pairing one), and flattening those into one rule would change behaviour.
	//
	// TestDeployStepsWriteTheCheckpointsTheyDeclare keeps the declaration honest.
	checkpoint string

	// skip is the step's precondition, lifted OUT of its body so the list can
	// say which steps a --db-only or box-reusing deploy does not run. Evaluated
	// immediately before the step, on the state as it stands then.
	skip func(*deployState) bool

	// run is the step. A method expression, so the list stays a pure value.
	run func(*Implementation, context.Context, *deployState) error
}

// deploySteps is the deploy, in order. Pure: it allocates a description and
// nothing else, so a test can read it without a machine, a network or a HOME.
//
// Read it as the answer to "what does a deploy do, and what does each part of it
// cost?" — the ordering rule being that everything free comes before anything
// that is not, and nothing that is not free goes unrecorded without a reason
// written down.
func deploySteps() []deployStep {
	return []deployStep{
		{
			name:    "resolve and configure",
			effects: effects(),
			run:     (*Implementation).stepResolveAndConfigure,
		},
		{
			name:    "decide box",
			effects: effects(),
			run:     (*Implementation).stepDecideBox,
		},
		{
			name:    "schema pre-flight",
			effects: effects(),
			// The gate exists to stop a deploy that would "create a database it
			// could never populate" — it renders the schema to prove the data
			// side is viable before anything is provisioned. With --skip-schema
			// there is no database being created or populated, so it is
			// guarding nothing, and a transient failure in the render service
			// would fail a deploy it has no stake in. (Observed: a server-side
			// "Failed to update execution" killed a run that was never going to
			// touch the database.)
			skip: func(st *deployState) bool { return st.s.SkipSchema },
			run:  (*Implementation).stepPreflightGate,
		},
		{
			name:    "generate app",
			effects: effects(effLocalFS),
			skip: func(st *deployState) bool {
				// --release-only re-releases the image and chart the last deploy
				// recorded, so there is nothing to regenerate.
				return st.dbOnly || (isK8s(st) && st.s.ReleaseOnly)
			},
			run: (*Implementation).stepGenerate,
		},
		{
			name:    "check cli release",
			effects: effects(),
			// The k8s path never installs the nuzur CLI anywhere: no agent runs
			// on the box, so no release has to exist for it.
			skip: isK8s,
			run:  (*Implementation).stepCheckCLIRelease,
		},
		{
			name:    "issue provisioning token",
			effects: effects(effCloud),
			// The token exists to pair an on-box agent headlessly. No agent, no
			// token — and minting one would leave a credential nothing consumes.
			skip: isK8s,
			run:  (*Implementation).stepIssueToken,
		},
		{
			name:       "record pending",
			effects:    effects(effRecord),
			checkpoint: deploy.StepPendingRecorded,
			// Managed providers only, and only when a VM is actually about to be
			// created: BYO-SSH and a reused box have no VM that could go missing.
			skip: func(st *deployState) bool {
				return !st.provider.CreatesInfrastructure() || st.reuseBox
			},
			run: (*Implementation).stepPendingRecord,
		},
		{
			name:       "provision",
			effects:    effects(effProvider, effRecord),
			checkpoint: deploy.StepInstanceCreated,
			run:        (*Implementation).stepProvision,
		},
		{
			name:       "record box",
			effects:    effects(effRecord),
			checkpoint: deploy.StepBoxRecorded,
			run:        (*Implementation).stepRecordBox,
		},
		{
			name:    "report in progress",
			effects: effects(effCloud),
			run:     (*Implementation).stepReportInProgress,
		},
		{
			name:    "provider firewall",
			effects: effects(effProvider),
			skip: func(st *deployState) bool {
				return !st.provider.CreatesInfrastructure() || st.prov.InstanceID == ""
			},
			run: (*Implementation).stepFirewall,
		},
		{
			name:    "ssh ping",
			effects: effects(),
			run:     (*Implementation).stepSSHPing,
		},
		// ── kubernetes ────────────────────────────────────────────────
		// Resolve the release's names and prove the cluster is reachable
		// BEFORE the chart is stamped, committed or built. Free, and it is
		// the check most likely to fail on a first run.
		{
			name:    "resolve cluster",
			effects: effects(),
			skip:    notK8s,
			run:     (*Implementation).stepK8sResolve,
		},
		{
			// Offers to write the host's credentials file from the resolved
			// connection. Before anything is generated or built, so a run that
			// still needs the file says so at the start.
			name:    "write host config",
			effects: effects(effBox),
			skip:    notK8s,
			run:     (*Implementation).stepK8sWriteConfig,
		},
		{
			name:    "stamp chart version",
			effects: effects(effLocalFS),
			skip: func(st *deployState) bool {
				// --release-only re-releases what the last deploy recorded, so
				// there is no new chart version to mint.
				return notK8s(st) || st.s.ReleaseOnly
			},
			run: (*Implementation).stepK8sStampChart,
		},
		{
			name:    "commit and push",
			effects: effects(effLocalFS),
			skip: func(st *deployState) bool {
				return notK8s(st) || st.s.NoCommit || st.s.ReleaseOnly
			},
			run: (*Implementation).stepK8sCommitPush,
		},
		{
			name:    "wait for ci",
			effects: effects(),
			skip: func(st *deployState) bool {
				return notK8s(st) || st.s.NoWait || st.s.ReleaseOnly
			},
			run: (*Implementation).stepK8sWaitCI,
		},
		{
			name:    "resolve image",
			effects: effects(),
			skip:    notK8s,
			run:     (*Implementation).stepK8sResolveImage,
		},

		{
			name:    "copy source",
			effects: effects(effBox),
			skip:    func(st *deployState) bool { return st.dbOnly || isK8s(st) },
			run:     (*Implementation).stepCopySource,
		},
		{
			name:    "bootstrap",
			effects: effects(effBox, effCloud),
			// NEVER for k8s. Beyond installing a runtime the cluster does not
			// want, the bootstrap ends with `ufw --force enable`, whose
			// default-deny policy would sever the node's API server (16443),
			// kubelet (10250), etcd and the whole NodePort range — and the
			// teardown never turns it back off.
			skip: isK8s,
			run:  (*Implementation).stepBootstrap,
		},
		{
			name:       "wait for agent",
			effects:    effects(effCloud, effRecord),
			checkpoint: deploy.StepAgentPaired,
			// No agent on the k8s path: the app's database is external and
			// reached directly, so there is nothing for an on-box agent to
			// proxy. The schema push goes through a nuzur team connection
			// instead (--connection).
			skip: isK8s,
			run:  (*Implementation).stepWaitAgent,
		},
		{
			name:    "publish catalog",
			effects: effects(effCloud),
			skip:    isK8s,
			run:     (*Implementation).stepPublishCatalog,
		},
		// Sits here rather than where `bootstrap` does — the two are the
		// equivalent step for their provider — so the checkpoint it writes ranks
		// above `wait for agent`'s. The two never both run, so the position
		// between them is free; strictly increasing ranks are not.
		{
			name:       "helm release",
			effects:    effects(effBox, effCloud, effRecord),
			checkpoint: deploy.StepReleased,
			skip:       notK8s,
			run:        (*Implementation).stepK8sRelease,
		},
		{
			name:    "apply schema",
			effects: effects(effBox, effCloud),
			// Skipped when asked to leave the database alone, and — for k8s —
			// when there is nothing to target. A team connection is that
			// provider's only route to the database, since there is no agent to
			// push through; without one the local mode would address an agent
			// that was never deployed and fail with an error about a box, on a
			// provider that has no box.
			skip: func(st *deployState) bool {
				return st.s.SkipSchema || (isK8s(st) && !st.fromConnection)
			},
			run: (*Implementation).stepApplySchema,
		},
		{
			name:    "read back front door",
			effects: effects(),
			skip:    isK8s,
			run:     (*Implementation).stepReadbackURL,
		},
		{
			// The k8s equivalent: the address comes from the Service or Ingress,
			// not from a file the bootstrap wrote on the box.
			name:    "read back cluster address",
			effects: effects(),
			skip:    notK8s,
			run:     (*Implementation).stepK8sReadbackURL,
		},
		{
			name:       "finalize record",
			effects:    effects(effRecord),
			checkpoint: deploy.StepFinalized,
			run:        (*Implementation).stepFinalizeRecord,
		},
		{
			// After the record is finalized, so the file only ever describes a
			// deploy that actually shipped. Needs no checkpoint: effLocalFS is a
			// workspace on this machine the user can see, and re-running rewrites it.
			name:    "record deploy config",
			effects: effects(effLocalFS),
			run:     (*Implementation).stepWriteDeployLock,
		},
		{
			name:    "finalize revision",
			effects: effects(effCloud),
			run:     (*Implementation).stepFinalizeRevision,
		},
		{
			name:    "report",
			effects: effects(),
			run:     (*Implementation).stepReport,
		},
	}
}

type deployState struct {
	// ── settings + targeting ──────────────────────────────────────────────
	// Resolved once, before anything runs, from --deploy-config merged with the
	// CLI flags. Every step reads its inputs from here rather than from the
	// cli.Context, which is why a step can be run in a test at all.
	s           *deploySettings
	provider    deploy.Provider
	provisioner deploy.Provisioner
	targets     *runTargets
	dbOnly      bool

	// The database: self-hosted on the box, or an EXISTING one reached by
	// --db-dsn / --connection. The ext* fields are the parts of that DSN, and
	// connStore is the team connection's store uuid (only set for --connection),
	// which the remote sql-push extension needs to target the connection.
	dbDSN          string
	connFlag       string
	fromConnection bool
	externalDB     bool
	dbEngine       deploy.DBEngine
	extHost        string
	extPort        string
	extUser        string
	extPass        string
	extName        string
	extParams      string
	connStore      string

	// ── naming ────────────────────────────────────────────────────────────
	// Derived from the identifier, which is shared with --plan so a plan and the
	// deploy it previews always name the same database (see deploy_targeting.go).
	identifier string
	imageName  string
	dbName     string
	dbUser     string
	schema     string
	dbSchema   string
	connName   string
	host       string

	// ── which box ─────────────────────────────────────────────────────────
	// The decision that keys everything downstream: the shared agent, the
	// connection uuid, the deployment id, the pre-flight gate and the workspace
	// are all keyed on the host.
	box            boxDecision
	reuseBox       bool
	prior          *deploy.Deployment
	reuseAgentUUID string
	connUUID       string
	depID          string

	// ── codegen ───────────────────────────────────────────────────────────
	// Empty for --db-only, which self-hosts the database and the agent only.
	configValues map[string]interface{}
	sourceRoot   string
	workspaceDir string
	jwtAuth      bool
	s3Enabled    bool
	s3Region     string
	s3Bucket     string
	s3Key        string
	s3Secret     string

	// ── runtime ───────────────────────────────────────────────────────────
	// What the run learns once it starts touching things: the provisioning
	// token, the agents that existed before pairing, the box it got, the runner
	// onto it, the record and revision it wrote, and the outcome it reports.
	tokRes         *pb.IssueProvisioningTokenResponse
	existing       map[string]bool
	resourceName   string
	prov           deploy.Provisioned
	target         deploy.Target
	runner         deploy.RemoteRunner
	dep            *deploy.Deployment
	reportIn       deploymentReportInput
	agentUUID      string
	publicURL      string
	useHTTPS       bool
	grpcTarget     string
	dataManagerURL string
	outcome        deployOutcome

	// ── kubernetes ────────────────────────────────────────────────────────
	// Only populated for ProviderK8s. The release is addressed by
	// namespace+name; the chart is generated locally (codegen emits it into the
	// workspace) and copied to the box, where helm runs.
	tools  deploy.ClusterTools
	appDir string // the generated app dir: <workspace>/<identifier>, and the git repo
	// deployLockPath is where stepWriteDeployLock put the lockfile, or "" if it
	// wrote none; the report reads it back.
	deployLockPath string
	chartDir       string // local chart dir inside the app dir
	chartVersion   string
	releaseName    string
	namespace      string
	imageRepo      string
	imageRef       string // the exact repository:tag or repository@digest deployed
	gitRoot        string // repo the workspace lives in (may be an ancestor of it)
	commitSHA      string

	// ── the interrupt cell ────────────────────────────────────────────────
	// Read from the signal-handling goroutine while the deploy runs, so every
	// access goes through the mutex. Deliberately the ONLY shared state: the
	// interrupt handler stays outside the step list (it has to fire between
	// steps as well as inside one), and this is the whole of what it needs.
	revMu          sync.Mutex
	deployRevUUID  string
	deployUserID   string
	pendingVMName  string
	lastCheckpoint string
}

// deployRev is the nuzur-side revision opened once the deploy is recorded (right
// after the box exists). Empty until then, which is how the interrupt handler
// knows whether there is anything to mark failed.
func (st *deployState) deployRev() string {
	st.revMu.Lock()
	defer st.revMu.Unlock()
	return st.deployRevUUID
}

func (st *deployState) setDeployRev(v string) {
	st.revMu.Lock()
	st.deployRevUUID = v
	st.revMu.Unlock()
}

// setDeployUserID records the id the user types into `nuzur-cli destroy` — known
// before the revision is, so the interrupt path can name it.
func (st *deployState) setDeployUserID(v string) {
	st.revMu.Lock()
	st.deployUserID = v
	st.revMu.Unlock()
}

func (st *deployState) deployUserIDVal() string {
	st.revMu.Lock()
	defer st.revMu.Unlock()
	return st.deployUserID
}

// setPendingVM is called once a managed VM may exist. From that instant an
// interrupt has to tell the user a server is running and how to remove it — the
// one thing that costs real money if they don't hear it.
func (st *deployState) setPendingVM(v string) {
	st.revMu.Lock()
	st.pendingVMName = v
	st.revMu.Unlock()
}

func (st *deployState) pendingVM() string {
	st.revMu.Lock()
	defer st.revMu.Unlock()
	return st.pendingVMName
}

// noteCheckpoint mirrors, in memory, a checkpoint the record is being given.
// Called from INSIDE the mutator that writes it — next to the assignment rather
// than after the call returns — so the cell tracks exactly the five points the
// record does and cannot drift from them as the pipeline is edited. (A write
// that then fails is handled at its own site: fatal where the record has to be
// believed, a warning where it need not be.)
//
// Its only reader is the interrupt handler, which until now could say a deploy
// was interrupted but not how much of one there was to clean up. The record
// already knows; this is how the message gets to say it too, without the handler
// reading a file from a signal goroutine.
func (st *deployState) noteCheckpoint(step string) {
	st.revMu.Lock()
	st.lastCheckpoint = step
	st.revMu.Unlock()
}

func (st *deployState) checkpoint() string {
	st.revMu.Lock()
	defer st.revMu.Unlock()
	return st.lastCheckpoint
}

// stepPublishCatalog (10a) puts the box's database in nuzur's catalog. Kept
// separate from the schema apply below because the two are independent, and
// folding them together once meant a publish failure silently cost you the
// schema as well.
func (i *Implementation) stepPublishCatalog(ctx context.Context, st *deployState) error {
	// 10. Publish the connection catalog (needs the user token — the box can't) and
	// auto-apply the schema to the empty DB. Two independent steps, tracked and
	// reported separately: a failure in one must neither skip nor be mistaken for
	// the other.
	// appShipped: the bootstrap above has already rebuilt the image and restarted the
	// container, so from here on an unapplied schema means the running app no longer
	// matches its database. --db-only has no app, so it has nothing to mismatch.
	st.outcome = deployOutcome{catalogPublished: true, schema: schemaStateApplied, appShipped: !st.dbOnly}
	i.updateDeployRevision(ctx, st.deployRev(),
		nemgen.DeploymentRevisionStatus_DEPLOYMENT_REVISION_STATUS_IN_PROGRESS, "publishing the connection to nuzur")
	if err := i.publishConnectionCatalog(st.agentUUID, st.connUUID, st.connName, st.dbEngine); err != nil {
		st.outcome.catalogPublished = false
		outputtools.PrintlnColoredErr("Connection not published to nuzur: "+err.Error(), outputtools.Yellow)
	}
	return nil
}

// stepApplySchema (10b) applies this project version's schema to the deployed
// database through the agent, and classifies what happened — applied, blocked by
// the destructive gate, or failed before/during the SQL. The classification is
// the whole output of this step: everything the user is told at the end reads it.
func (i *Implementation) stepApplySchema(ctx context.Context, st *deployState) error {
	i.updateDeployRevision(ctx, st.deployRev(),
		nemgen.DeploymentRevisionStatus_DEPLOYMENT_REVISION_STATUS_IN_PROGRESS, "applying the schema to the database")
	pushTarget := deployPushTarget(st.agentUUID, st.connUUID, st.schema, st.connFlag, st.connStore, st.dbEngine)
	var gate schemaGateResult
	applyErr := i.applySchema(st.targets, pushTarget, st.s.AllowDestructive, &gate)
	st.outcome.schema = classifySchemaOutcome(applyErr, gate)
	switch st.outcome.schema {
	case schemaStateApplied:
		if gate.destructiveApplied {
			st.outcome.destructiveApplied = true
			st.outcome.destructiveCount = len(gate.plan.Destructive())
		}
	case schemaStateBlocked:
		st.outcome.destructiveCount = len(gate.plan.Destructive())
		st.outcome.rerunCommand = rerunCommand(os.Args, true)
	default:
		// Not "skipped": skipping is what the gate does, and calling both the same
		// thing is how the louder problem ended up sounding like the quieter one.
		outputtools.PrintlnColoredErr("Schema apply FAILED: "+applyErr.Error(), outputtools.Red)
		if st.outcome.schema == schemaStateFailedDuringApply {
			// SQL reached the database, so whether the failure took the rest of the
			// migration back with it is the question. Only claimed when the plan that
			// was attempted is in hand — an error before the confirmation step leaves
			// gate.plan empty, and an empty plan must not be read as "nothing could
			// have landed".
			st.outcome.schemaRolledBack = !gate.plan.Empty() &&
				gate.plan.Transactional(sqlplan.Engine(st.dbEngine))
		}
	}
	return nil
}

// stepReadbackURL (10c + 11) reads the front door the bootstrap actually chose
// and builds the data-manager link from it. Read back rather than composed,
// because the public port is allocated ON THE BOX so N projects can coexist.
func (i *Implementation) stepReadbackURL(ctx context.Context, st *deployState) error {
	// Read back the resolved front-door URL the bootstrap wrote: a domain project
	// → https://{domain}; an IP-only project → http://{host}:{auto-assigned port}
	// (the public port is allocated on the box so N projects can coexist). Falls
	// back to a best-effort compose if the readback fails. --db-only has no front
	// door.
	st.publicURL, st.useHTTPS, st.grpcTarget = "", false, ""
	if !st.dbOnly {
		st.publicURL, _ = st.runner.Capture(ctx, "cat /etc/nuzur/"+st.identifier+"/url 2>/dev/null")
		st.publicURL = strings.TrimSpace(st.publicURL)
		if st.publicURL == "" {
			if st.s.Domain != "" {
				st.publicURL = "https://" + st.s.Domain
			} else {
				st.publicURL = "http://" + st.target.Host
			}
		}
		st.useHTTPS = strings.HasPrefix(st.publicURL, "https://")
		// gRPC dial target host:port (grpcurl needs an explicit port).
		st.grpcTarget = strings.TrimPrefix(strings.TrimPrefix(st.publicURL, "https://"), "http://")
		if !strings.Contains(st.grpcTarget, ":") {
			if st.useHTTPS {
				st.grpcTarget += ":443"
			} else {
				st.grpcTarget += ":80"
			}
		}
	}

	// 11. Build the data-manager deep link (opens the deployed DB directly,
	// with the local-agent connection preselected).
	st.dataManagerURL = fmt.Sprintf(
		"%s/project/data-manager/%s/%s?mode=local&localAgent=%s&localAgentConn=%s&schema=%s",
		strings.TrimRight(st.s.WebURL, "/"),
		st.targets.project.Uuid, st.targets.projectVersion.Uuid,
		st.agentUUID, st.connUUID, url.QueryEscape(st.schema),
	)
	return nil
}

// stepFinalizeRecord (12) completes the local record with what only exists now.
func (i *Implementation) stepFinalizeRecord(ctx context.Context, st *deployState) error {
	var err error
	// 12. Finalize the record: the row was written right after provisioning (6b)
	// so the box was never un-destroyable; fill in what only exists now that
	// pairing + the front door are up. A re-deploy updates the same ID in place.
	st.dep, err = deploy.MutateDeployment(st.depID, func(rec *deploy.Deployment) {
		rec.LocalAgentUUID = st.agentUUID
		rec.APIURL = st.publicURL
		rec.PublicURL = st.publicURL
		rec.DataManagerURL = st.dataManagerURL
		// The workspace, recorded HERE because this is the first point on every
		// path where it is actually known. stepRecordBox runs before the cluster
		// resolve, and stepPendingRecord — the only other writer — is skipped for
		// providers that create no infrastructure. So on the k8s path the workspace
		// was never recorded at all, however many times the deploy succeeded, and
		// every re-deploy had to be handed --source-dir by hand.
		recordBoxWorkspace(rec, st.workspaceDir)
		// The TEAM connection this deploy ran against (--connection), which is not
		// the same thing as ConnUUID: that one is an identity the CLI mints for
		// itself (uuid.NewV4, named <identifier>-db) and is meaningless to anyone
		// looking for a connection in nuzur. Without this the record knows the
		// database is external but not which connection reaches it, so the guard in
		// applyDeploymentSelector can only tell the user to remember — which is
		// exactly the dead end a re-deploy of aburrides hit.
		if st.connFlag != "" {
			rec.TeamConnUUID = st.connFlag
		}
		rec.LastCompletedStep = deploy.StepFinalized
		st.noteCheckpoint(deploy.StepFinalized)
		// This deployment is now in a good state, whatever the last run of it
		// ended with. A stale error left on a healthy record is a lie the next
		// run would read as fact.
		rec.LastError = ""
	})
	if err != nil {
		return err
	}
	return nil
}

// stepFinalizeRevision (12b) completes the nuzur-side revision opened at 6c and
// flips it ACTIVE. Best-effort: the local record is authoritative for destroy.
func (i *Implementation) stepFinalizeRevision(ctx context.Context, st *deployState) error {
	// 12b. Finalize the nuzur-side revision: fill in what only exists now (the
	// box-allocated ports, the front-door URL, the agent) and flip it ACTIVE, which
	// supersedes the previously-current revision. Updates the SAME revision opened
	// at 6c rather than stacking a duplicate. Best-effort: the local record is
	// authoritative for destroy, so a cloud hiccup must not fail a good deploy.
	st.reportIn.Runner = st.runner
	st.reportIn.LocalAgentUUID = st.agentUUID
	st.reportIn.PublicURL = st.publicURL
	st.reportIn.DataManagerURL = st.dataManagerURL
	st.reportIn.UseHTTPS = st.useHTTPS
	st.reportIn.RevisionUUID = st.deployRev()
	st.reportIn.ImageName = st.imageName // built by now — safe to pin in the history
	// ACTIVE even when step 10 was partial: the box, the front door and the app are
	// genuinely serving, so FAILED would mislabel a working deployment, and nem has
	// no DEGRADED value. The shortfall is recorded in the status message instead, so
	// the deployment history can tell a schema-less deploy from a clean one.
	st.reportIn.Status = nemgen.DeploymentRevisionStatus_DEPLOYMENT_REVISION_STATUS_ACTIVE
	st.reportIn.StatusMessage = st.outcome.revisionMessage()
	if _, err := i.reportDeployment(ctx, st.reportIn); err != nil {
		outputtools.PrintlnColoredErr("Deployment recorded locally but not reported to nuzur: "+err.Error(), outputtools.Yellow)
	}
	return nil
}

// stepReport (13) is everything the user is told once the deploy is over, and
// the exit code. It is the last step, and the only one whose error is the run's
// result rather than a failure.
func (i *Implementation) stepReport(ctx context.Context, st *deployState) error {
	// 13. Report.
	//
	// The banner is qualified rather than unconditional. It used to print "Deployment
	// complete." in green after a schema apply that had errored — so the last loud
	// thing on screen contradicted the one yellow line above it, and a failed
	// migration read as a clean deploy. Both halves are worth saying; saying only the
	// good half is what misleads.
	// notAttempted counts as complete: --skip-schema (or a k8s deploy with no
	// team connection) means the schema step was never meant to run, so calling
	// that "the schema was NOT applied" reports an instruction as a failure.
	if st.outcome.schema == schemaStateApplied || st.outcome.schema == schemaStateNotAttempted {
		outputtools.PrintlnColored("\nDeployment complete.", outputtools.Green)
	} else {
		outputtools.PrintlnColoredErr("\nDeployment finished, but the schema was NOT applied — see the summary below.", outputtools.Red)
	}
	fmt.Fprintf(outputtools.Stdout, "  deployment id: %s\n", st.dep.ID)
	// Only when there is one. The k8s path deploys no agent, and a blank value
	// beside a label reads as something that failed to resolve rather than
	// something that does not exist here.
	if strings.TrimSpace(st.agentUUID) != "" {
		fmt.Fprintf(outputtools.Stdout, "  agent uuid:    %s\n", st.agentUUID)
	}
	fmt.Fprintf(outputtools.Stdout, "  connection:    %s (%s)\n", st.connName, st.connUUID)
	if st.externalDB {
		fmt.Fprintf(outputtools.Stdout, "  database:      external %s at %s:%s/%s (not self-hosted; kept on destroy)\n", st.dbEngine, st.extHost, st.extPort, st.dbName)
	}
	fmt.Fprintf(outputtools.Stdout, "  teardown:      nuzur-cli destroy %s\n", st.dep.ID)

	if st.dbOnly {
		// Database-only: no app, no front door — just the database managed
		// through nuzur via the agent connection.
		outputtools.PrintlnColored("\nWhat's deployed (database-only):", outputtools.Green)
		if st.externalDB {
			fmt.Fprintf(outputtools.Stdout, "  Database:  external %s (%s:%s), schema applied via the agent.\n", st.dbEngine, st.extHost, st.extPort)
		} else {
			fmt.Fprintf(outputtools.Stdout, "  Database:  self-hosted %s on the box (localhost), schema applied.\n", st.dbEngine)
		}
		fmt.Fprintf(outputtools.Stdout, "  Managed:   through nuzur — data manager, SQL Push, and queries via the agent.\n")

		// Loud, actionable notice: db-only is a materially different outcome from
		// a normal deploy (no HTTP API at all), and users who just said "deploy my
		// database" often still expect the generated API. Make the consequence
		// impossible to miss and give the one-step way to add it.
		outputtools.PrintlnColoredErr("\n  This was a DATABASE-ONLY deploy — no REST/gRPC API or app was created.", outputtools.Yellow)
		fmt.Fprintf(outputtools.Stdout, "  Nothing serves this data over HTTP; it is reachable only through nuzur (data manager, SQL Push, queries).\n")
		fmt.Fprintf(outputtools.Stdout, "  If you also want nuzur's generated API in front of this database, re-run the same deploy\n")
		fmt.Fprintf(outputtools.Stdout, "  WITHOUT --db-only (the database, agent, schema and data are reused) — for example:\n")
		rerun := "nuzur-cli deploy"
		if p := strings.TrimSpace(st.s.Provider); p != "" {
			rerun += " --provider " + p
		}
		if h := strings.TrimSpace(st.s.Host); h != "" {
			rerun += " --host " + h
		}
		if pr := strings.TrimSpace(st.s.Project); pr != "" {
			rerun += " --project " + pr
		}
		rerun += " --version " + st.targets.projectVersion.Uuid + " --api both"
		fmt.Fprintf(outputtools.Stdout, "    %s\n", rerun)
		fmt.Fprintf(outputtools.Stdout, "  (add your original --ssh-key / --auth / --domain flags as needed).\n")
	} else {
		// What's deployed: this project's own Caddy front door (HTTPS via a domain,
		// otherwise plain HTTP on its auto-assigned public port).
		// Name the front door this provider actually uses. There is no Caddy on
		// the k8s path — traffic arrives through the cluster's ingress — and
		// telling someone to look at a reverse proxy that is not there is the
		// kind of detail that sends them debugging the wrong machine.
		frontDoor := "Caddy"
		if isK8s(st) {
			frontDoor = "the cluster ingress"
		}
		scheme := "HTTP"
		if st.useHTTPS {
			scheme = "HTTPS"
		}
		outputtools.PrintlnColored(fmt.Sprintf("\nWhat's deployed (%s via %s):", scheme, frontDoor), outputtools.Green)
		// grpcTarget is derived from the box's own ports; the k8s path routes
		// gRPC through the ingress instead, so it can be empty. Printing the
		// line anyway produced `grpcurl -plaintext  list` — a command with no
		// target that cannot work and does not say why.
		if boolValue(st.configValues, "grpc_server_enabled") && strings.TrimSpace(st.grpcTarget) != "" {
			if st.useHTTPS {
				fmt.Fprintf(outputtools.Stdout, "  gRPC API:  %s (TLS)\n", st.grpcTarget)
				fmt.Fprintf(outputtools.Stdout, "             grpcurl %s list\n", st.grpcTarget)
			} else {
				fmt.Fprintf(outputtools.Stdout, "  gRPC API:  %s (plaintext)\n", st.grpcTarget)
				fmt.Fprintf(outputtools.Stdout, "             grpcurl -plaintext %s list\n", st.grpcTarget)
			}
		}
		if boolValue(st.configValues, "rest_enabled") {
			base := stringValue(st.configValues, "rest_base_path", "/v1")
			fmt.Fprintf(outputtools.Stdout, "  REST API:  %s%s\n", st.publicURL, base)
			fmt.Fprintf(outputtools.Stdout, "             curl %s%s/<entity>\n", st.publicURL, base)
		}
		if st.jwtAuth {
			fmt.Fprintf(outputtools.Stdout, "  Auth:      jwt — data endpoints need a Bearer token.\n")
			fmt.Fprintf(outputtools.Stdout, "             sign in: POST %s/signin {\"email\",\"password\"} (then /refresh, /validate)\n", st.publicURL)
			where := "on the box"
			if isK8s(st) {
				where = "on the cluster host, in the credentials file"
			}
			fmt.Fprintf(outputtools.Stdout, "             a signing key was generated %s; sign-in needs a user row in your user entity.\n", where)
		}
		fmt.Fprintf(outputtools.Stdout, "  Info page: %s/\n", st.publicURL)
		if g := strings.TrimSpace(st.s.GRPCDomain); g != "" {
			fmt.Fprintf(outputtools.Stdout, "  gRPC API:  %s (its own hostname, on its own Ingress)\n", g)
		}
		// gRPC over plain HTTP does not work, and nothing downstream says so.
		//
		// ingress-nginx enables HTTP/2 only on its TLS listener — `listen 443 ssl
		// http2`, while `listen 80` has no http2 — because a gRPC client negotiates
		// HTTP/2 through ALPN, which only exists in the TLS handshake. So without a
		// certificate the Ingress is created, carries every correct annotation, and
		// still cannot serve gRPC: clients fail to negotiate, and a plain HTTP/1.1
		// request gets proxied through to the gRPC server, which answers 415.
		//
		// Said separately from the HTTP notice below because the consequence is not
		// the same. Plain HTTP is a downgrade for REST; for gRPC it is not working
		// at all, which is worth more than a parenthetical.
		if isK8s(st) && !st.useHTTPS && strings.TrimSpace(st.s.GRPCDomain) != "" {
			outputtools.PrintlnColoredErr(fmt.Sprintf(
				"\nwarning: %s is routed but will NOT serve gRPC until it has TLS.\n"+
					"  ingress-nginx only enables HTTP/2 on its TLS listener, and gRPC clients negotiate\n"+
					"  HTTP/2 through ALPN — which only happens in a TLS handshake. Over plain HTTP a client\n"+
					"  cannot connect at all, and curl gets 415 from the gRPC server behind it.\n"+
					"  Add a cert: a cert-manager cluster-issuer annotation, or a tls block, on grpcIngress.",
				strings.TrimSpace(st.s.GRPCDomain)), outputtools.Yellow)
		}
		if !st.useHTTPS && !isK8s(st) {
			outputtools.PrintlnColoredErr("  (IP-only deploy over plain HTTP — pass --domain <name> for automatic HTTPS with a trusted cert.)", outputtools.Yellow)
		} else if !st.useHTTPS && strings.TrimSpace(st.s.Domain) != "" {
			// The hostname is served, but nothing has issued a certificate for
			// it yet. Telling a user who DID pass --domain to pass --domain is
			// the wrong advice — on this path TLS comes from the cluster's
			// ingress and cert-manager, not from the deploy.
			outputtools.PrintlnColoredErr(
				"  (Serving plain HTTP for now — add a TLS block to the ingress values, or a cert-manager\n"+
					"   cluster-issuer annotation, for HTTPS. The hostname itself is already routed.)",
				outputtools.Yellow)
		}
	}

	// Only when there is somewhere to send them. The k8s path registers no agent
	// connection, so this URL is empty — and a "Manage your data:" heading over
	// a blank line reads as something that failed to load.
	if strings.TrimSpace(st.dataManagerURL) != "" {
		outputtools.PrintlnColored("\nManage your data:", outputtools.Green)
		fmt.Fprintf(outputtools.Stdout, "  %s\n", st.dataManagerURL)
	}
	if st.outcome.catalogPublished {
		fmt.Fprintf(outputtools.Stdout, "  The connection is listed under \"Via agent\" — nuzur reaches it through the agent on this box,\n")
		fmt.Fprintf(outputtools.Stdout, "  which dials out to nuzur. The database stays private; nothing is exposed to the internet.\n")
	}
	// Say only what actually failed. This block used to assert a cause ("the diff
	// step errored") that was wrong whenever the publish was what broke, and to
	// claim the agent connection was live in exactly the case where it wasn't.
	if s := st.outcome.summary(); s != "" {
		outputtools.PrintlnColoredErr("\n"+s, st.outcome.summaryColor())
	}
	printGateFollowUp(st.outcome)

	// Point the user at their editable app source (the workspace) — this is the
	// code that was deployed. Re-running deploy regenerates it in place, refreshing
	// generated code while keeping their custom endpoints, then ships it.
	if st.workspaceDir != "" {
		appDir := st.sourceRoot // the project dir (go.mod/Dockerfile); may be nested under the workspace
		if appDir == "" {
			appDir = st.workspaceDir
		}
		outputtools.PrintlnColored("\nYour app source:", outputtools.Green)
		fmt.Fprintf(outputtools.Stdout, "  %s\n", appDir)
		fmt.Fprintf(outputtools.Stdout, "  Re-run the same deploy to ship changes from here.\n")
		// How to deploy this again from anywhere — a different machine, a teammate,
		// CI — without knowing any of the flags this run was given.
		if advice := deployLockAdvice(st.deployLockPath); advice != "" {
			fmt.Fprintf(outputtools.Stdout, "%s\n", advice)
		}
		// Resolved, not the flag: the tip is about the code that was just generated.
		if boolValue(st.configValues, "custom_enabled") {
			fmt.Fprintf(outputtools.Stdout, "  Add custom endpoints: edit app/grpc.go (override/extend gRPC) or app/rest.go\n")
			fmt.Fprintf(outputtools.Stdout, "  (custom REST routes); add RPCs in app/idl/proto/custom.proto then run app/idl/proto/gen.sh.\n")
		}
		// Pointless on the k8s path, which just committed and pushed this very
		// directory — telling someone to `git init` a repo you have already
		// pushed from reads as the deploy not knowing what it did.
		if !isK8s(st) {
			fmt.Fprintf(outputtools.Stdout, "  Tip: run `git init` here (or commit) to track your changes and see what codegen\n")
			fmt.Fprintf(outputtools.Stdout, "  refreshes each deploy — secrets are already covered by the generated .gitignore.\n")
		}
	}

	// Optionally register a raw --db-dsn database as a team connection so the whole
	// team can use the data manager on it. Opt-in only (flag or TTY prompt), and
	// skipped for --connection (already a team connection) and self-hosted DBs
	// (unreachable from nuzur cloud). Best-effort — never fails the deploy.
	if st.s.SaveConnection && (!st.externalDB || st.fromConnection) {
		outputtools.PrintlnColoredErr("--save-connection applies only to an external --db-dsn deploy; ignoring.", outputtools.Yellow)
	}
	if st.externalDB && !st.fromConnection && shouldSaveTeamConnection(st.s.NoSaveConnection, st.s.SaveConnection) {
		i.saveTeamConnection(saveConnectionInput{
			TeamUUID:    st.targets.project.TeamUuid,
			ProjectName: st.targets.project.Name,
			Identifier:  st.identifier,
			Engine:      st.dbEngine,
			Host:        st.extHost,
			Port:        st.extPort,
			User:        st.extUser,
			Pass:        st.extPass,
			Name:        st.extName,
			Params:      st.extParams,
		})
	}

	// Last: a schema that did not reach the database exits non-zero — blocked or
	// failed — so CI does not go green on a box that is serving against a schema its
	// generated code no longer matches. Everything above has already printed, because
	// the deploy itself did happen.
	return exitCodeForOutcome(st.outcome)
}

// stepPendingRecord (6-pre) writes the deployment record BEFORE the VM is
// created, reserving the provider resource name. Managed providers only: for
// BYO-SSH and a reused box there is no VM to lose track of.
func (i *Implementation) stepPendingRecord(ctx context.Context, st *deployState) error {
	var err error
	// 6-pre. Mint the provider-side resource name HERE rather than inside the adapter, and
	// write it to local state before the create call. Creating a VM is a side effect
	// we cannot make atomic with recording it, so the record goes first: if this
	// process dies any time after the call starts, `nuzur-cli destroy <id>` can still
	// find the VM — by id once we have it, by name until then. Without this a killed
	// deploy left a running, billing VM that nothing on disk pointed at.
	st.resourceName, err = deploy.ProviderResourceName(st.identifier)
	if err != nil {
		return err
	}
	if _, err := deploy.MutateDeployment(st.depID, func(rec *deploy.Deployment) {
		rec.Provider = st.provider
		rec.ProviderResourceName = st.resourceName
		rec.Provisioning = true
		rec.Region = st.s.Region
		rec.Identifier = st.identifier
		rec.ProjectUUID = st.targets.project.Uuid
		rec.ProjectVersionUUID = st.targets.projectVersion.Uuid
		rec.DBEngine = st.dbEngine
		// The workspace ROOT, not sourceRoot: resolveWorkspace reads this
		// back on the next run, and a deploy that died after this write
		// used to hand it the app dir — nesting the retry's generated
		// workspace inside the previous app.
		rec.WorkspaceDir = st.workspaceDir
		rec.CreatedAt = time.Now().UTC()
		rec.LastCompletedStep = deploy.StepPendingRecorded
		st.noteCheckpoint(deploy.StepPendingRecorded)
	}); err != nil {
		return fmt.Errorf("recording the deploy before creating the server: %w", err)
	}
	st.setPendingVM(st.resourceName)
	return nil
}

// stepProvision (6) gets the box: BYO-SSH validates the host, a managed provider
// creates the VM over its own CLI and waits for SSH, and a reused box is rebuilt
// from the record. Everything after the returned Target is provider-agnostic.
func (i *Implementation) stepProvision(ctx context.Context, st *deployState) error {
	var err error
	// 6. Provision: BYO-SSH validates the host; a managed provider creates the VM
	// (over its own CLI) and waits for SSH. Everything after the returned Target is
	// provider-agnostic.
	spec := deploy.Spec{
		Provider: st.provider,
		Target: deploy.Target{
			Host: st.s.Host, User: st.s.User,
			Port: st.s.Port, KeyPath: st.s.SSHKey,
		},
		ProviderConfig: deploy.ProviderConfig{
			Region:     st.s.Region,
			Size:       st.s.Size,
			Image:      st.s.Image,
			SSHKeyName: st.s.SSHKeyName,
		},
		Identifier:         st.identifier,
		ProjectUUID:        st.targets.project.Uuid,
		ProjectVersionUUID: st.targets.projectVersion.Uuid,
		DBEngine:           st.dbEngine,
		ProvisioningToken:  st.tokRes.GetProvisioningToken(),
		SourceDir:          st.sourceRoot,
		ResourceName:       st.resourceName,
		// Fires the moment the provider acknowledges the VM — minutes before
		// Provision returns, since it still has to wait for SSH. Persist the id now
		// so the box is deletable for that whole wait.
		//
		// A failure to record is warned about whatever caused it. It used to
		// return silently when the record could not be LOADED, and warn only
		// when it could not be written — so the one case where the instance id
		// is lost AND nothing on disk explains why was the quiet one.
		OnInstanceCreated: func(ref deploy.InstanceRef) {
			if _, err := deploy.MutateDeployment(st.depID, func(rec *deploy.Deployment) {
				rec.ProviderInstanceID = ref.InstanceID
				rec.Region = ref.Region
				if ref.Host != "" {
					rec.Host = ref.Host
				}
				rec.LastCompletedStep = deploy.StepInstanceCreated
				st.noteCheckpoint(deploy.StepInstanceCreated)
			}); err != nil {
				outputtools.PrintlnColoredErr(fmt.Sprintf(
					"warning: created %s instance %s but could not record it locally (%v) — delete it manually if this deploy fails",
					st.provider, ref.InstanceID, err), outputtools.Yellow)
			}
		},
	}
	switch {
	case st.reuseBox:
		// The box already exists and was reached above, so there is nothing to
		// provision and nothing to wait for. Rebuild the Provisioned the rest of the
		// deploy expects from the RECORD, so the provider ids survive: they are the
		// only handle `nuzur-cli destroy` has on the VM, and re-recording this
		// deployment without them would leave the droplet running with nothing on
		// disk pointing at it.
		st.prov = deploy.Provisioned{
			Target:     deploy.Target{Host: st.host, User: st.s.User, Port: st.s.Port, KeyPath: st.s.SSHKey},
			InstanceID: st.box.Record.ProviderInstanceID,
			Region:     st.box.Record.Region,
		}
		st.resourceName = st.box.Record.ProviderResourceName
	default:
		if st.provider.CreatesInfrastructure() {
			outputtools.PrintlnColoredErr("Creating the server on "+string(st.provider)+" (this can take a minute)...", outputtools.Blue)
		}
		st.prov, err = st.provisioner.Provision(ctx, spec)
		if err != nil {
			return err
		}
	}
	st.target = st.prov.Target
	// Managed providers create the host, so --host (and thus `host`) was empty.
	// Adopt the provisioned address so the bootstrap URL, ports readback, public
	// URL, and deployment record all use the real VM IP.
	if strings.TrimSpace(st.host) == "" {
		st.host = st.target.Host
	}
	return nil
}

// stepRecordBox (6b) records the deployment as soon as the box exists, so an
// interrupt from here on leaves something `nuzur-cli destroy` can clean up.
// recordBoxWorkspace writes the resolved workspace onto the record, and only when
// this run actually resolved one.
//
// An empty value here never means "this deployment has no workspace" — it means
// this run had not worked out where it is yet, and on the k8s path that is the
// NORMAL case rather than an error: stepPendingRecord is skipped wherever the
// provider creates no infrastructure, `generate app` is skipped by --release-only,
// and the cluster resolve runs after the box is recorded.
//
// Assigning unconditionally therefore ERASED the path the record already held. The
// next run then falls back to ./nuzur-<identifier> relative to wherever it happens
// to be invoked from and dies with "locating the generated app under …", for a
// deployment whose workspace was never in doubt. Same hazard as the LocalAgentUUID
// carry-forward, and the same fix: a field the run cannot speak to is a field the
// run must not overwrite.
func recordBoxWorkspace(rec *deploy.Deployment, resolved string) {
	if resolved == "" {
		return
	}
	rec.WorkspaceDir = resolved
}

func (i *Implementation) stepRecordBox(ctx context.Context, st *deployState) error {
	var err error
	// 6b. Record the deployment AS SOON AS THE BOX EXISTS — before the long
	// bootstrap/build/pairing steps. If anything after this fails (or the run is
	// interrupted), the record still carries the provider instance id, so
	// `nuzur-cli destroy <id>` can tear the VM down instead of orphaning a billing
	// server nuzur has no memory of. Step 12 fills in the rest (agent, URLs).
	//
	// The record a decision was taken from is only this deployment's when the box is
	// being REUSED. A --new-vm run also carries one — the box it is billing alongside
	// — and that box's agent belongs to that box, not to the fresh VM being created
	// here.
	var adoptedRecord *deploy.Deployment
	if st.reuseBox {
		adoptedRecord = st.box.Record
	}
	//
	// A MERGE, not a replacement. Every field below is one this step genuinely
	// knows; anything else the record already carries survives — which is how the
	// front-door URLs a previous deploy of this box recorded stop being blanked
	// for the ~20 minutes between here and step 12. (No user-visible change: the
	// URLs printed at the end are read back from the box either way. What changes
	// is what `deploy list` and `--plan --deployment <id>` see if the run is
	// interrupted in that window.)
	st.dep, err = deploy.MutateDeployment(st.depID, func(rec *deploy.Deployment) {
		rec.Provider = st.provider
		rec.ProviderInstanceID = st.prov.InstanceID
		// Carried forward from the pre-provision record: dropping the name would
		// lose the only handle on a VM whose id never came back.
		rec.ProviderResourceName = st.resourceName
		// Explicitly cleared rather than left to the merge: the box exists now,
		// and a record still marked mid-provision makes destroy skip the on-box
		// teardown and decideDeployBox refuse to provision past it.
		rec.Provisioning = false
		rec.Region = st.prov.Region
		rec.Host, rec.User, rec.Port = st.target.Host, st.target.User, st.target.Port
		rec.Identifier = st.identifier
		rec.ProjectUUID = st.targets.project.Uuid
		rec.ProjectVersionUUID = st.targets.projectVersion.Uuid
		rec.ConnUUID = st.connUUID
		rec.DBEngine = st.dbEngine
		rec.ExternalDB = st.externalDB
		recordBoxWorkspace(rec, st.workspaceDir)
		rec.Domain = st.s.Domain
		// The other two hostnames of this deployment, written in the same breath
		// as the first. They are what the NEXT run adopts when it does not state
		// them (applyDeploymentSelector), which is what stops a re-deploy that
		// forgot a flag from taking the auth or gRPC front door down.
		rec.AuthDomain = st.s.AuthDomain
		rec.GRPCDomain = st.s.GRPCDomain
		// Written from what this run KNOWS rather than left to the merge: on a
		// --new-vm run the id being written to may be a fresh one with nothing in
		// it, and blanking the agent uuid for the ~20 minutes between here and
		// step 12 is not a cosmetic loss. It makes `--plan --deployment <id>` fail
		// with a false diagnosis ("the deploy that created it did not finish
		// pairing" — it had), and if the re-deploy is interrupted in that window
		// the loss is permanent: pickPriorDeployment skips agentless records, so
		// the next deploy mints a SECOND record for the same host+identifier,
		// which is what makes destroy's isLast refuse to delete the VM.
		// Empty on a genuine first deploy, where no agent is known yet — that
		// record correctly reads as "died before pairing" until step 9 fills it in.
		rec.LocalAgentUUID = knownAgentUUID(st.prior, st.reuseAgentUUID, adoptedRecord)
		rec.CreatedAt = time.Now().UTC()
		switch {
		case st.prior != nil:
			rec.CreatedAt = st.prior.CreatedAt
			carryForwardProvisioning(rec, st.prior, st.provider)
		case adoptedRecord != nil:
			// Adopting a died-in-flight record (no agent, so `prior` skipped it):
			// its creation time is when this deployment really started.
			rec.CreatedAt = adoptedRecord.CreatedAt
		}
		rec.LastCompletedStep = deploy.StepBoxRecorded
		st.noteCheckpoint(deploy.StepBoxRecorded)
	})
	if err != nil {
		return err
	}
	return nil
}

// stepReportInProgress (6c) makes the deploy visible in nuzur while the slow
// bootstrap/build/pair steps run, and opens the revision the rest of the run
// updates.
func (i *Implementation) stepReportInProgress(ctx context.Context, st *deployState) error {
	// 6c. Report the deploy to nuzur as IN_PROGRESS — same reasoning as the local
	// record above, for the cloud side: the box exists, so it should be visible
	// (and watchable, and seen failing) while the slow bootstrap/build/pair steps
	// run. Everything except the box-allocated ports, URLs and agent is already
	// known; step 12b finalizes THIS revision with the rest. Best-effort: progress
	// reporting must never fail an otherwise-good deploy.
	st.reportIn = deploymentReportInput{
		// dep.Provider, not provider: on an SSH re-deploy onto a managed box this is
		// the carried-forward original, so the cloud record keeps saying digitalocean
		// instead of flipping to ssh.
		Provider:       st.dep.Provider,
		Identifier:     st.identifier,
		ProjectUUID:    st.targets.project.Uuid,
		ProjectVersion: st.targets.projectVersion.Uuid,
		ConnUUID:       st.connUUID,
		Host:           st.target.Host,
		DBEngine:       st.dbEngine,
		ExternalDB:     st.externalDB,
		DBOnly:         st.dbOnly,
		Domain:         st.s.Domain,
		GRPCDomain:     st.s.GRPCDomain,
		AuthDomain:     st.s.AuthDomain,
		ExtDBPort:      st.extPort,
		RESTEnabled:    boolValue(st.configValues, "rest_enabled"),
		GRPCEnabled:    boolValue(st.configValues, "grpc_server_enabled"),
		JWTAuth:        st.jwtAuth,
		AuthConfig:     stringValue(st.configValues, "auth", ""),
		Region:         st.s.Region,
		Size:           st.s.Size,
		Image:          st.s.Image,
		SSHKeyName:     st.s.SSHKeyName,
		SSHUser:        st.target.User,
		SSHPort:        st.target.Port,
		DBSchema:       st.schema,
		// The RESOLVED value, not the flag: with --custom sticky the flag may well be
		// absent on a deploy that does generate the custom zone, and the deployment
		// history should record what shipped rather than what was typed.
		Custom:        boolValue(st.configValues, "custom_enabled"),
		SourceDir:     st.workspaceDir,
		Status:        nemgen.DeploymentRevisionStatus_DEPLOYMENT_REVISION_STATUS_IN_PROGRESS,
		StatusMessage: "server ready — bootstrapping",
	}
	if rev, err := i.reportDeployment(ctx, st.reportIn); err != nil {
		outputtools.PrintlnColoredErr("Deploy not reported to nuzur (continuing): "+err.Error(), outputtools.Yellow)
	} else {
		st.setDeployRev(rev)
	}
	return nil
}

// stepFirewall (6d) restricts inbound at the provider level to mirror the box's
// own ufw.
func (i *Implementation) stepFirewall(ctx context.Context, st *deployState) error {
	// Restrict inbound at the provider level to mirror the box's ufw (SSH + the
	// Caddy front doors). Best-effort — the on-box ufw is the authoritative gate,
	// so a firewall hiccup must not fail an otherwise-good deploy. No-op for BYO-SSH.
	// Re-run on a reused box too (a re-deploy can open a new project's port), except
	// when the record never learned the instance id and there is nothing to address.
	if err := st.provisioner.ConfigureFirewall(ctx, st.prov, deployFirewallRules(st.dbOnly, st.s.Domain)); err != nil {
		outputtools.PrintlnColoredErr("Provider firewall not fully configured (the box's own ufw still applies): "+err.Error(), outputtools.Yellow)
	}
	return nil
}

// stepSSHPing (6e) opens the SSH session the rest of the deploy runs over.
func (i *Implementation) stepSSHPing(ctx context.Context, st *deployState) error {
	st.runner = i.sshRunner(st.target)
	// Non-root SSH users need sudo for the privileged bootstrap steps.
	st.runner.SetSudo(st.s.Sudo || st.target.User != "root")
	outputtools.PrintlnColoredErr(i.localize.Localize("deploy_preflight", "Checking SSH connectivity..."), outputtools.Blue)
	if err := st.runner.Ping(ctx); err != nil {
		return err
	}
	return nil
}

// stepCopySource (7) copies the generated source to the box.
func (i *Implementation) stepCopySource(ctx context.Context, st *deployState) error {
	// 7. Copy generated source to a user-writable path (scp runs as the SSH
	// user, which may be non-root; the sudo bootstrap builds from here). Skipped
	// for --db-only (no app to build).
	if err := st.runner.RunCommand(ctx, "rm -rf "+remoteSrcDir); err != nil {
		return err
	}
	outputtools.PrintlnColoredErr(i.localize.Localize("deploy_copying", "Copying source to the server..."), outputtools.Blue)
	if err := st.runner.CopyDir(ctx, st.sourceRoot, remoteSrcDir); err != nil {
		return err
	}
	return nil
}

// stepBootstrap (8) renders and runs the bootstrap: Docker, the database, the
// image build, Caddy and the agent pairing. Idempotent by design — every "the
// next run recovers" property of this pipeline leans on that.
func (i *Implementation) stepBootstrap(ctx context.Context, st *deployState) error {
	// 8. Render + run the bootstrap.
	// Empty cli-install-cmd → the bootstrap installs the nuzur CLI from GitHub
	// releases itself, PINNED to this CLI's own version (see
	// BootstrapParams.CLIVersion): the box then runs exactly the CLI that is driving
	// the deploy, and a release published while the deploy runs cannot change what
	// the box downloads out from under it.
	bp := deploy.BootstrapParams{
		Identifier:        st.identifier,
		DBEngine:          st.dbEngine,
		DBName:            st.dbName,
		DBUser:            st.dbUser,
		DBOnly:            st.dbOnly,
		ExternalDB:        st.externalDB,
		DBHost:            st.extHost,
		DBPort:            st.extPort,
		DBPassword:        st.extPass,
		DBParams:          st.extParams,
		DBDSN:             st.dbDSN,
		DBSchema:          st.dbSchema,
		GRPCEnabled:       boolValue(st.configValues, "grpc_server_enabled"),
		JWTAuth:           st.jwtAuth,
		ProvisioningToken: st.tokRes.GetProvisioningToken(),
		CLIInstallCmd:     st.s.CLIInstallCmd,
		CLIVersion:        constants.CLI_VERSION,
		ConnUUID:          st.connUUID,
		ConnName:          st.connName,
		Domain:            st.s.Domain,
		// Optional extra Caddy sites, alongside the one Domain names. Empty for
		// almost every deploy, and the snippet is byte-identical when they are.
		AuthDomain: st.s.AuthDomain,
		GRPCDomain: st.s.GRPCDomain,
		Host:       st.host,
		S3Enabled:  st.s3Enabled,
		S3Region:   st.s3Region,
		S3Bucket:   st.s3Bucket,
		S3Key:      st.s3Key,
		S3Secret:   st.s3Secret,
	}
	if !st.dbOnly {
		bp.RemoteSrcDir = remoteSrcDir
		bp.ImageName = st.imageName
	}
	script, err := deploy.RenderBootstrap(bp)
	if err != nil {
		return err
	}
	dbLabel := "MySQL"
	if st.dbEngine == deploy.DBPostgres {
		dbLabel = "Postgres"
	}
	bootMsg := "Bootstrapping the server (Docker, " + dbLabel + ", build, pairing)..."
	if st.dbOnly {
		bootMsg = "Bootstrapping the server (" + dbLabel + " + agent, database-only)..."
	}
	outputtools.PrintlnColoredErr(i.localize.Localize("deploy_bootstrapping", bootMsg), outputtools.Blue)
	i.updateDeployRevision(ctx, st.deployRev(),
		nemgen.DeploymentRevisionStatus_DEPLOYMENT_REVISION_STATUS_IN_PROGRESS, bootMsg)
	if err := st.runner.RunScript(ctx, deploy.ScriptBootstrap, script); err != nil {
		return err
	}
	return nil
}

// stepWaitAgent (9 + 9b) waits for the box's agent to come online and records
// the pairing, which is the checkpoint that stops the next run from mistaking a
// working deployment for one that died before pairing.
func (i *Implementation) stepWaitAgent(ctx context.Context, st *deployState) error {
	var err error
	// 9. Verify the agent connected. First deploy → a new agent UUID appears;
	// re-deploy → the existing (reused) agent should come back ONLINE.
	outputtools.PrintlnColoredErr(i.localize.Localize("deploy_verifying", "Waiting for the agent to connect..."), outputtools.Blue)
	i.updateDeployRevision(ctx, st.deployRev(),
		nemgen.DeploymentRevisionStatus_DEPLOYMENT_REVISION_STATUS_IN_PROGRESS, "waiting for the agent to connect")
	var online bool
	if st.reuseAgentUUID != "" {
		st.agentUUID = st.reuseAgentUUID
		online, err = i.waitForAgentOnline(st.reuseAgentUUID, 150*time.Second)
	} else {
		st.agentUUID, online, err = i.waitForNewOnlineAgent(st.existing, 150*time.Second)
	}
	if err != nil {
		return err
	}
	if !online {
		outputtools.PrintlnColoredErr("Agent registered but not observed online yet; schema auto-apply may fail until it connects.", outputtools.Yellow)
	}
	// 9b. Record the pairing NOW rather than at step 12.
	//
	// On a first deploy the agent uuid does not exist until this line, so the
	// record written at 6b correctly had none — and every run that dies between
	// here and step 12 used to leave a record that still reads "died before
	// pairing". pickPriorDeployment skips such records, so the next deploy mints a
	// SECOND record for the same box, and destroy's isLast then refuses to delete
	// the VM. One small write closes that window.
	//
	// Best-effort: the deployment is working, the agent is paired, and step 12's
	// write is the one that must be believed. Failing the run here would throw
	// away a good deploy over a checkpoint.
	if _, mErr := deploy.MutateDeployment(st.depID, func(rec *deploy.Deployment) {
		rec.LocalAgentUUID = st.agentUUID
		rec.LastCompletedStep = deploy.StepAgentPaired
		st.noteCheckpoint(deploy.StepAgentPaired)
	}); mErr != nil {
		outputtools.PrintlnColoredErr(fmt.Sprintf(
			"warning: paired agent %s but could not record it locally (%v)", st.agentUUID, mErr), outputtools.Yellow)
	}
	return nil
}

// stepResolveAndConfigure (0e-3a) is the deploy's whole resolution phase: the
// provider, the database (self-hosted or external), the project version and the
// go-code-gen extension, the generator config assembled from flags + the
// project's saved config, and the names everything downstream is keyed on.
//
// Deliberately ONE step rather than five. Its parts are not a sequence, they are
// a fixpoint: the identifier comes out of the generator config, the generator
// config's `db` comes from the engine, the engine may come from the record found
// by an identifier, and the S3 block is resolved from a config value that the
// same block may have written. Slicing that into steps would produce steps that
// cannot run in any order but one and cannot be understood apart — the appearance
// of structure over the same coupling.
//
// What it has in common is the property that matters to the step list: it touches
// nothing. Everything here is a read, a computation or a message; the deploy can
// still be abandoned at the end of it with nothing to clean up.
func (i *Implementation) stepResolveAndConfigure(ctx context.Context, st *deployState) error {
	var err error
	st.provider = deploy.Provider(strings.TrimSpace(st.s.Provider))
	if st.provider == "" {
		st.provider = deploy.ProviderSSH
	}
	st.provisioner, err = i.provisioner(st.provider)
	if err != nil {
		return err
	}
	if st.provider.UsesGivenHost() && strings.TrimSpace(st.s.Host) == "" {
		return fmt.Errorf("--host is required for the %s provider", st.provider)
	}
	st.dbOnly = st.s.DBOnly

	// --db-dsn / --connection: connect to an EXISTING database instead of
	// self-hosting one. --db-dsn takes a raw DSN; --connection resolves the DSN
	// server-side from a stored team connection (no plaintext secret on the CLI).
	// Both feed the same external-DB path below.
	st.dbDSN = strings.TrimSpace(st.s.DBDSN)
	st.connFlag = strings.TrimSpace(st.s.Connection)
	if st.connFlag != "" && st.dbDSN != "" {
		return fmt.Errorf("--connection and --db-dsn are mutually exclusive")
	}
	st.fromConnection = st.connFlag != ""
	st.externalDB = st.dbDSN != "" || st.fromConnection
	st.dbEngine = deploy.DBMySQL
	// connStore is the team connection's store uuid (only set for --connection);
	// the remote sql-push extension needs it to target the connection.
	if st.dbDSN != "" {
		var perr error
		st.dbEngine, st.extHost, st.extPort, st.extUser, st.extPass, st.extName, st.extParams, perr = parseDeployDSN(st.dbDSN)
		if perr != nil {
			return fmt.Errorf("parsing --db-dsn: %w", perr)
		}
		if st.extName == "" {
			return fmt.Errorf("--db-dsn must include a database name")
		}
	} else if !st.fromConnection && st.s.DB == "postgres" {
		// Self-hosted Postgres: install + provision PG on the box (parallels the
		// MySQL local tier). The engine drives the bootstrap install/create branch,
		// the app config driver, and the agent connection's --driver/--schema.
		st.dbEngine = deploy.DBPostgres
	}

	// 1. Resolve project/version + the go-code-gen extension (logs in).
	st.targets, err = i.resolveRunTargets(extRunFlags{
		project:        st.s.Project,
		version:        st.s.Version,
		nonInteractive: true,
	}, deployResolveOptions())
	if err != nil {
		return err
	}

	// --connection: resolve the DSN parts from the stored team connection now that
	// the project's team is known. Drives the same external-DB path as --db-dsn.
	if st.fromConnection {
		st.dbEngine, st.extHost, st.extPort, st.extUser, st.extPass, st.extName, st.extParams, st.connStore, err = i.resolveConnectionForDeploy(st.connFlag, st.targets.project.TeamUuid)
		if err != nil {
			return err
		}
	}

	// --host re-deploy with no --db: take the engine from the record.
	//
	// `deploy --plan` reads the engine off the deployment record
	// (planTargetFromDeployment); the deploy read it off --db, which DEFAULTS to
	// mysql. So planning a Postgres box without repeating --db diffed schema
	// `public` and then handed the user a command that re-deployed that same box as
	// MySQL: a different schema, a mysql branch in the bootstrap and the app
	// config, and — when the plan was destructive — an --allow-destructive pointed
	// at a database nobody had diffed. deploy_targeting.go's header promises the two
	// sides reach the SAME answer; this is the dimension no shared helper covered.
	//
	// The same adoption applyDeploymentSelector already performs for --deployment,
	// on the selector that had none. Conditions, in order: nothing stated an engine
	// (see deploySettings.DBStated); the database is self-hosted, so there IS an
	// engine to choose (--db-dsn and --connection carry their own); a --host was
	// given; and the record for that host+identifier recorded an engine that
	// contradicts the default — the only case in which the record is news.
	//
	// The identifier is derived the way --plan derives it, from the project's saved
	// generator config, because that is the value the two sides have to agree on
	// and it is settled before the deploy resolves its own config (see
	// planIdentifier).
	//
	// Scope, stated because it is narrower than it looks: a MANAGED re-deploy
	// passes no --host (the box decision below finds it), so it is not covered
	// here. Nor does it need to be for the agreement this fixes — `--plan` with no
	// --host resolves no live target at all and reports create-mode, so there are
	// not two answers to disagree.
	if !st.s.DBStated && !st.externalDB {
		if host := strings.TrimSpace(st.s.Host); host != "" {
			priorDeps, _ := deploy.ListDeployments()
			priorRec := pickPriorDeployment(priorDeps, host, planIdentifier(st.s.Identifier, lastGoCodeGenConfig(st.targets), st.targets.project.Name))
			if priorRec != nil && priorRec.DBEngine != "" && priorRec.DBEngine != st.dbEngine {
				st.dbEngine = priorRec.DBEngine
				st.s.DB = string(priorRec.DBEngine)
				outputtools.PrintlnColoredErr(fmt.Sprintf(
					"Deploying to deployment %s on %s, taken from its record: db=%s. Any flag you passed overrides it.",
					priorRec.ID, host, priorRec.DBEngine), outputtools.Blue)
			}
		}
	}

	// 2 + 3. Generate the app (skipped entirely for --db-only, which self-hosts
	// only the DB + agent and manages it through nuzur — no app, no code-gen
	// config required, so it works for any project).
	st.jwtAuth = false
	// S3 storage: resolved from the team ObjectStore referenced by --storage (or
	// the object_store saved in the project's go-code-gen config). Enables the
	// generated /upload + /sign endpoints and is written into the box's prod.yaml.
	if !st.dbOnly {
		// The go-code-gen config: the deploy-config's `codegen` block overlaid by a
		// --gen-config file (resolved in s.Codegen), then the deploy-level knobs
		// (db/custom/api/auth) applied on top.
		provided := map[string]interface{}{}
		for k, v := range st.s.Codegen {
			provided[k] = v
		}
		// dbEngine is authoritative (from --db, or inferred from --db-dsn). go-code-gen's
		// `db` config option uses "postgresql" (its DatabaseType enum) — distinct from the
		// runtime driver name "postgres" used in prod.yaml + the agent connection.
		provided["db"] = goCodeGenDBValue(st.dbEngine)
		// Written ONLY when the user said something about it this run. `provided`
		// always beats the project's saved config, so writing it unconditionally made
		// --custom a flag you had to re-pass on every single deploy: omitting it
		// regenerated the app with the custom zone off, which drops the custom-routes
		// hook from the generated server and `app.ProvideCustomRoutes` from main.go.
		// The user's own app/ package survives on disk but is no longer imported, so
		// `go build .` never compiles it — the deploy succeeds and every custom
		// endpoint is simply gone. Leaving the key out lets the saved config carry the
		// setting forward, which is what "re-run the same deploy" has to mean.
		if st.s.Custom != nil {
			provided["custom_enabled"] = *st.s.Custom
		}
		provided["dockerfile"] = true
		// Transport selection: pick REST for JS/web clients, gRPC for Go/backend
		// clients. Unset leaves the project's last/provided config untouched.
		switch st.s.API {
		case "rest":
			provided["proto_enabled"] = false
			provided["grpc_server_enabled"] = false
			provided["rest_enabled"] = true
		case "grpc":
			provided["proto_enabled"] = true
			provided["grpc_server_enabled"] = true
			provided["rest_enabled"] = false
		case "both":
			provided["proto_enabled"] = true
			provided["grpc_server_enabled"] = true
			provided["rest_enabled"] = true
		case "":
			// leave to codegen / last-used / generator defaults
		default:
			return fmt.Errorf("--api must be one of: rest, grpc, both")
		}
		if a := st.s.Auth; a != "" {
			provided["auth"] = a
		}
		// Storage: --storage-enabled, --storage <uuid>, or any --s3-* flag turns the
		// generation switch on (the saved config's storage_enabled/object_store flow
		// through lastConfig otherwise). --storage overrides the saved object store.
		manualS3 := strings.TrimSpace(st.s.S3Bucket) != ""
		if st.s.StorageEnabled || strings.TrimSpace(st.s.Storage) != "" || manualS3 {
			provided["storage_enabled"] = true
		}
		if ref := strings.TrimSpace(st.s.Storage); ref != "" {
			provided["object_store"] = ref
		}
		// Fill the required generator fields nothing else supplies. A project that
		// has never had go-code-gen run against it (created via the API/MCP, or
		// straight from the web) has no last-used config, and the deploy flags cover
		// only part of the generator's required surface — so without this the very
		// first deploy of a new project failed validation on `identifier`,
		// `go_module`, `events`, … before anything was provisioned. Missing fields
		// only: explicit values and the saved config always win.
		codegenIdentifier := sanitizeDBName(firstNonEmpty(st.s.Identifier, st.targets.project.Name))
		// An explicitly passed --identifier names the generated root folder and go
		// module too, per the flag's own help — which it did not do on a project that
		// already had a saved go-code-gen config, because the identifier only ever
		// reached the generator as a default for a MISSING field. Nothing about which
		// deployment record this run matches changes; see applyCodegenIdentity.
		if renamed := applyCodegenIdentity(provided, st.targets.lastConfig, st.s.Identifier); len(renamed) > 0 {
			outputtools.PrintlnColoredErr(fmt.Sprintf(
				"Naming the generated app from --identifier: %s (the project's saved go-code-gen config said identifier=%s).",
				strings.Join(renamed, ", "), stringValue(st.targets.lastConfig, "identifier", "")), outputtools.Blue)
		}
		// The k8s path releases a Helm chart built by a generated workflow, so
		// both must be generated whatever the project's saved config says.
		// Applied BEFORE the defaults pass, which only fills missing fields and
		// would leave an explicit `helm: false` in place.
		if st.provider == deploy.ProviderK8s {
			if forced := applyK8sCodegenRequirements(provided, st.targets.lastConfig); len(forced) > 0 {
				var reasons []string
				for _, f := range forced {
					reasons = append(reasons, fmt.Sprintf("%s (%s)", f, k8sRequiredCodegen[f]))
				}
				outputtools.PrintlnColoredErr(fmt.Sprintf(
					"Turning on generator options the k8s provider requires: %s.",
					strings.Join(reasons, "; ")), outputtools.Blue)
			}
		}
		if applied := applyCodegenDefaults(st.targets.configEntity, provided, st.targets.lastConfig, codegenIdentifier); len(applied) > 0 {
			lead := "No saved go-code-gen config for this project (first deploy)"
			if len(st.targets.lastConfig) > 0 {
				lead = "The project's saved go-code-gen config is missing required fields"
			}
			outputtools.PrintlnColoredErr(fmt.Sprintf(
				"%s — deploying with derived defaults: %s.\nOverride with the deploy flags (--identifier/--api/--auth/--db), a `codegen` block in --deploy-config, or --gen-config <file>.\nThe resolved config is saved as this project's go-code-gen config, so later deploys reuse it.",
				lead, strings.Join(applied, ", ")), outputtools.Yellow)
		}

		// The three hostnames, in both directions: adopted from the project's saved
		// config when this run supplies none, and written back when it does — so the
		// generated values.yaml carries the real hosts and the config, not a local
		// deployment record, is where they live.
		if adopted := resolveCodegenDomains(st.s, provided, st.targets.lastConfig); len(adopted) > 0 {
			outputtools.PrintlnColoredErr(fmt.Sprintf(
				"Using the hostnames saved in the project's go-code-gen config: %s.",
				strings.Join(adopted, ", ")), outputtools.Blue)
		}

		if err := checkDistinctDomains(st.s); err != nil {
			return err
		}

		st.configValues, err = st.targets.er.BuildConfigFromJSON(st.targets.project, st.targets.projectVersion.Uuid, st.targets.configEntity, provided, st.targets.lastConfig)
		if err != nil {
			return fmt.Errorf("building generator config — supply the missing fields via --gen-config <file> or a `codegen` block in --deploy-config, or run `nuzur-cli go-code-gen` once to save a config for this project: %w", err)
		}

		// The custom zone is sticky when the flag is omitted (see deploySettings.Custom).
		// Say so at the point it is resolved: a setting that carries itself forward in
		// silence is undiscoverable, and the user who wants it off has to be told how.
		if notice := customStickinessNotice(st.s.Custom, provided, st.targets.lastConfig); notice != "" {
			outputtools.PrintlnColoredErr(notice, outputtools.Blue)
		}

		// Catch an unsupportable JWT config here: left alone it generates fine and
		// only fails on the remote host, during the docker build, after the VPS has
		// already been provisioned.
		configErr, jwtWarnings, checkErr := st.targets.er.ValidateJWTAuthRequirements(st.targets.projectVersion.Uuid, st.configValues)
		if checkErr != nil {
			return checkErr
		}
		if configErr != nil {
			return configErr
		}
		for _, w := range jwtWarnings {
			outputtools.PrintlnColoredErr(fmt.Sprintf("warning: %s", w), outputtools.Yellow)
		}

		// If storage generation is on, resolve credentials for prod.yaml — from a
		// team ObjectStore (nuzur-stored) or the manual --s3-* flags. Storage may be
		// enabled with no creds yet (endpoints return 503 until prod.yaml is set).
		if boolValue(st.configValues, "storage_enabled") || strings.TrimSpace(stringValue(st.configValues, "object_store", "")) != "" {
			storeUUID := strings.TrimSpace(stringValue(st.configValues, "object_store", ""))
			switch {
			case storeUUID != "":
				st.s3Region, st.s3Bucket, st.s3Key, st.s3Secret, err = i.resolveObjectStoreForDeploy(storeUUID, st.targets.project.TeamUuid)
				if err != nil {
					return err
				}
				st.s3Enabled = true
			case manualS3:
				st.s3Region, st.s3Bucket, st.s3Key, st.s3Secret = strings.TrimSpace(st.s.S3Region), strings.TrimSpace(st.s.S3Bucket), strings.TrimSpace(st.s.S3AccessKey), st.s.S3Secret
				st.s3Enabled = true
			default:
				outputtools.PrintlnColoredErr("S3 storage is enabled but no credentials were provided (no --storage / --s3-* and none saved) — /upload and /sign will return 503 until you set the aws: block in the app's prod.yaml.", outputtools.Yellow)
			}
		}
		// Generation happens below (step 2), once the identifier + any prior
		// deployment are known — so it targets the persistent workspace.
	}

	// Identifier: --identifier override, else the go-code-gen config's identifier,
	// else (db-only) the sanitized project name. Shared with --plan so a plan and
	// the deploy it previews always name the same database — see deploy_targeting.go.
	st.identifier = planIdentifier(st.s.Identifier, st.configValues, st.targets.project.Name)

	// Per-revision image tag: each deploy builds + runs a uniquely-tagged image
	// (not :latest) so the deployment revision history pins the exact artifact
	// that shipped — the basis for auditing and a future rollback.
	st.imageName = fmt.Sprintf("nuzur/%s:%s", st.identifier, time.Now().UTC().Format("20060102-150405")+"-"+shortID()[:6])

	// The DB is registered as a named agent connection with this UUID, then
	// published to nuzur so the schema can be pushed to it. Self-hosted → a DB
	// named after the identifier with a least-priv `{db}_app` user; external
	// (--db-dsn) → the DB name + user from the DSN.
	st.dbName = sanitizeDBName(st.identifier)
	st.dbUser = st.dbName + "_app"
	if st.externalDB {
		// external DB name/user come from the DSN/connection. A MySQL connection is
		// server-level (no database name), so fall back to the identifier-derived
		// name — the app targets that database on the connection's server.
		if st.extName != "" {
			st.dbName = st.extName
		}
		st.dbUser = st.extUser
	}
	if st.externalDB && st.extName == "" {
		st.extName = st.dbName
	}
	// --connection has no raw DSN yet: assemble one from the resolved parts so the
	// external-DB bootstrap can inject it into the on-box agent connection.
	if st.fromConnection {
		st.dbDSN = assembleDeployDSN(st.dbEngine, st.extHost, st.extPort, st.extUser, st.extPass, st.extName, st.extParams)
	}
	// Schema vs database: in MySQL the database IS the schema; in Postgres a
	// database contains schemas (default `public`). `schema` is what the diff
	// engine, the data-manager link, and the agent connection's default schema
	// target — the DB name for MySQL, a namespace for Postgres.
	st.schema = deploySchemaName(st.dbEngine, st.dbName, st.s.DBSchema)
	st.dbSchema = "" // agent-connection default schema; empty for MySQL (chosen per query)
	if st.dbEngine == deploy.DBPostgres {
		st.dbSchema = st.schema
	}
	st.connName = st.identifier + "-db"
	st.host = st.s.Host
	return nil
}

// stepDecideBox (3b-3e) picks the box and, with it, this deployment's identity:
// the prior record, the box's shared agent, the connection uuid and the
// deployment id. Ends by proving a reused box actually answers — the check that
// keeps a re-deploy from quietly provisioning a replacement.
func (i *Implementation) stepDecideBox(ctx context.Context, st *deployState) error {
	var err error
	// WHICH BOX. For --provider ssh this is just --host. For a managed provider it
	// is the decision that used to not exist: a re-deploy of the same project +
	// identifier reuses the VM the provider already created for it instead of
	// creating (and billing for) another one. See decideDeployBox for the matrix.
	//
	// Taken HERE, before the prior-deployment lookup, because everything downstream
	// — the shared agent, the connection uuid, the deployment id, the destructive
	// pre-flight gate, the workspace — is keyed on the host. Resolving the host
	// first is what makes managed re-deploys preserve exactly what SSH re-deploys
	// already preserved.
	allDeployments, _ := deploy.ListDeployments()
	st.box, err = decideDeployBox(boxDecisionInput{
		Provider:    st.provider,
		HostFlag:    st.s.Host,
		NewVM:       st.s.NewVM,
		Identifier:  st.identifier,
		ProjectUUID: st.targets.project.Uuid,
		Deployments: allDeployments,
	})
	if err != nil {
		return err
	}
	if st.box.Message != "" {
		colour := outputtools.Blue
		if st.box.Action == boxProvision && st.box.Record != nil {
			// A fresh VM alongside one that already exists is the billing case; it
			// should not read like routine progress.
			colour = outputtools.Yellow
		}
		outputtools.PrintlnColoredErr(st.box.Message, colour)
	}
	st.reuseBox = st.box.Action == boxReuseRecorded
	if st.reuseBox {
		// From here the run is indistinguishable from `--provider ssh --host <recorded>`:
		// the recorded SSH parameters, no provisioning, the same idempotent bootstrap.
		st.host = st.box.Host
		st.s.Host = st.box.Host
		if st.box.User != "" {
			st.s.User = st.box.User
		}
		if st.box.Port != 0 {
			st.s.Port = st.box.Port
		}
		// A managed re-deploy passes no --region (the box already exists), so keep
		// reporting the one the VM actually lives in rather than blanking it.
		if strings.TrimSpace(st.s.Region) == "" {
			st.s.Region = st.box.Record.Region
		}
	}

	// Multi-project on one box: the box has ONE shared agent (reused for every
	// project on it — box-level), while the connection UUID + deployment record
	// are per-project (host+identifier).
	st.prior = pickPriorDeployment(allDeployments, st.host, st.identifier)
	// Guard: refuse if this identifier on this host maps to a DIFFERENT project —
	// they'd share the derived DB name/user and collide. Require a distinct id.
	if st.prior != nil && st.prior.ProjectUUID != "" && st.prior.ProjectUUID != st.targets.project.Uuid {
		return fmt.Errorf("host %s already runs a different project under identifier %q (project %s) — deploy the new project under a distinct identifier", st.host, st.identifier, st.prior.ProjectUUID)
	}
	st.reuseAgentUUID = pickBoxAgent(allDeployments, st.host)
	st.connUUID = ""
	if st.prior != nil {
		st.connUUID = st.prior.ConnUUID
	}
	if st.connUUID == "" {
		connU, err := uuid.NewV4()
		if err != nil {
			return err
		}
		st.connUUID = connU.String()
	}
	if st.reuseAgentUUID != "" {
		outputtools.PrintlnColoredErr("Reusing the box's existing agent ("+st.reuseAgentUUID+") — no new pairing.", outputtools.Blue)
	}

	// Deployment id: reuse the prior record on a re-deploy, else mint one now. The
	// record is written as soon as the box exists (step 6b) rather than at the end,
	// so an interrupted deploy still leaves something `nuzur-cli destroy` can clean up.
	st.depID = st.identifier + "-" + shortID()
	switch {
	case st.prior != nil:
		st.depID = st.prior.ID
	case st.reuseBox && st.box.Record != nil:
		// Adopting a box whose record has no agent — the deploy that created it died
		// before pairing, which is exactly what pickPriorDeployment skips. Write back
		// onto THAT record rather than minting a second one for the same VM: two
		// records pointing at one box is how the orphan was created in the first
		// place, and this run is finishing the job the dead one started.
		st.depID = st.box.Record.ID
	}
	st.setDeployUserID(st.depID)

	// A reused box has to answer before anything is generated or reported. If it
	// doesn't, the deploy stops: silently provisioning a replacement is the exact
	// behaviour this reuse exists to remove.
	if st.reuseBox {
		outputtools.PrintlnColoredErr("Checking the reused server "+st.host+" is reachable...", outputtools.Blue)
		probe := i.sshRunner(deploy.Target{Host: st.host, User: st.s.User, Port: st.s.Port, KeyPath: st.s.SSHKey})
		if err := probe.Ping(ctx); err != nil {
			return reusedBoxUnreachableError(st.box.Record, st.provider, st.identifier, err)
		}
	}
	return nil
}

// stepPreflightGate (1b + 1c) is the schema pre-check, in its two mirror-image
// forms: a re-deploy has a live database, so the destructive gate computes a real
// diff against it; a first deploy has none, so the CREATE script is rendered
// instead. Both run while nothing has been shipped, which is the entire point —
// the apply is step 10, and a gate that refuses there refuses too late.
func (i *Implementation) stepPreflightGate(ctx context.Context, st *deployState) error {
	// 1b. RE-DEPLOY ONLY: run the destructive gate before anything is shipped.
	//
	// The apply is step 10, after the bootstrap has rebuilt the image and restarted
	// the container — so a gate that refuses there refuses too late, leaving new code
	// serving the old schema. Here the box, its agent and its database already exist
	// (that is what `prior` means), so the migration is computable while nothing has
	// changed yet, and a blocked deploy ships nothing.
	//
	// Skipped when --allow-destructive is set: the gate would let it through anyway,
	// and the pre-flight costs a round trip to the box's agent. Skipped on a first
	// deploy, which has no agent yet and no old schema to mismatch. Non-blocking on
	// any error — see preflightSchemaGate.
	if !st.s.AllowDestructive && st.prior != nil && st.prior.ConnUUID != "" {
		if preflightAgent := firstNonEmpty(st.prior.LocalAgentUUID, st.reuseAgentUUID); preflightAgent != "" {
			preflightTarget := deployPushTarget(preflightAgent, st.prior.ConnUUID, st.schema, st.connFlag, st.connStore, st.dbEngine)
			if err := i.preflightSchemaGate(st.targets, preflightTarget); err != nil {
				return err
			}
		}
	}

	// 1c. FIRST DEPLOY ONLY: can this project's schema be rendered at all?
	//
	// The mirror image of the gate above. A re-deploy has a live database, so the
	// pre-flight computes a real diff; a first deploy has none, and the schema is
	// not looked at until step 10 — by which point a server exists, is billing, and
	// is running an application whose database was never created. The same render
	// `deploy --plan` performs answers the question here for the price of one
	// read-only sql-gen run, while nothing has been provisioned.
	//
	// Two outcomes, and the difference is the whole design:
	//
	//   - the render FAILS: block. The apply at step 10 would fail the same way, and
	//     failing now costs nothing.
	//   - the project has NO standalone entities: warn and continue. That is not a
	//     broken schema, it is an entity-less project, which deploys perfectly well
	//     (the app, the box and the agent are all still wanted) and would apply an
	//     empty migration.
	//
	// Runs for --db-only too. Its condition is not "this deploy ships an app" but
	// "this deploy will create a schema on a database that does not exist yet",
	// which is exactly what --db-only is FOR — the schema is that deploy's entire
	// deliverable, so an unrenderable one there is worth more, not less.
	//
	// Skipped on a re-deploy (prior != nil) and on an adopted box (reuseBox): both
	// mean a database is already there, and both are the case the pre-flight gate
	// above already covers with a real diff. Cost: one sql-gen run, on first
	// deploys only.
	if st.prior == nil && !st.reuseBox {
		if err := i.checkCreatePlanRenders(st.targets, st.dbEngine); err != nil {
			switch {
			case errors.Is(err, errNoStandaloneEntities):
				outputtools.PrintlnColoredErr(fmt.Sprintf(
					"warning: %v. The server, the database and the agent will still be created, and the schema apply will have nothing to do.", err),
					outputtools.Yellow)
			default:
				return fmt.Errorf(
					"this project version's schema cannot be rendered, so the deploy would create a database it could never populate: %w\n"+
						"Nothing has been provisioned — no server, no database, no deployment record. Fix the schema (`nuzur-cli deploy --plan` renders the same script) and re-run",
					err)
			}
		}
	}
	return nil
}

// stepGenerate (2) generates the app into the persistent workspace and saves the
// config that generated it as the project's go-code-gen config.
func (i *Implementation) stepGenerate(ctx context.Context, st *deployState) error {
	var err error
	// 2. Generate the app into the PERSISTENT workspace (full-app deploys only) —
	// the editable source of truth deploy builds from. Re-deploys regenerate in
	// place, refreshing generated code while preserving the user's custom
	// endpoints (see extensionrun's user-file-preserving extraction).
	st.workspaceDir, err = resolveWorkspace(st.s.SourceDir, st.prior, st.identifier)
	if err != nil {
		return err
	}

	// Refuse before anything is written or saved, if the chart about to be
	// regenerated was not generated in the first place. Placed here rather than
	// beside applyK8sCodegenRequirements because it must cover a `helm: true`
	// arriving from anywhere, not only the k8s provider's force-on — and because
	// the workspace is only resolved now.
	if boolValue(st.configValues, "helm") {
		if appDir, err := findSourceRoot(st.workspaceDir); err == nil {
			if err := checkHandMaintainedChart(filepath.Join(appDir, ".helm", st.identifier)); err != nil {
				return err
			}
		}
	}

	outputtools.PrintlnColoredErr("Generating application code into "+st.workspaceDir+" ...", outputtools.Blue)
	if _, err := st.targets.er.Run(extensionrun.RunParams{
		Extension:          st.targets.extension,
		ExtensionVersion:   st.targets.extensionVersion,
		ProjectUUID:        st.targets.project.Uuid,
		ProjectVersionUUID: st.targets.projectVersion.Uuid,
		ConfigValues:       st.configValues,
		OutputPath:         st.workspaceDir,
	}); err != nil {
		return fmt.Errorf("generating code: %w", err)
	}
	// Remember the config that just generated as the project's last-used
	// go-code-gen config — the same record `nuzur-cli go-code-gen` writes and the
	// web app reads. Without this a deploy-derived config was invisible and
	// unreusable: the next deploy re-derived it, a later `go-code-gen` run still
	// prompted from scratch, and one-off flags (--api/--auth) silently reverted.
	// Saved AFTER generation succeeded, so a config that cannot even generate
	// never becomes what the project remembers. Non-fatal: the deploy already has
	// what it needs.
	//
	// ANNOUNCED, because it is the one thing a deploy does to state that outlives
	// the deploy and belongs to another command. A user who runs `deploy --api
	// both --auth jwt` once and `nuzur-cli go-code-gen` later gets the deploy's
	// config, and nothing said so: the write was invisible, which made it read as
	// a bug in whichever command showed the surprise. The line is printed only on
	// success — the failure path keeps its warning, and neither claims the other's
	// outcome.
	if saveErr := st.targets.er.SaveLastUsedConfigEntry(st.targets.projectVersion.Uuid, st.targets.extension.Identifier, st.configValues); saveErr != nil {
		outputtools.PrintlnColoredErr(fmt.Sprintf("warning: could not save this deploy's generator config for reuse: %v", saveErr), outputtools.Yellow)
	} else {
		outputtools.PrintlnColoredErr(
			"Saved this deploy's generator config as the project's go-code-gen config (later deploys and the web app reuse it).",
			outputtools.Blue)
	}
	st.sourceRoot, err = findSourceRoot(st.workspaceDir)
	if err != nil {
		return err
	}
	st.jwtAuth = generatedHasJWTAuth(st.sourceRoot)
	// Ignore files go at the project root (where the Dockerfile + go.mod live,
	// which the generator nests under <identifier>) — that's the docker build
	// context root and the natural `git init` root.
	if gerr := writeWorkspaceGitignore(st.sourceRoot); gerr != nil {
		outputtools.PrintlnColoredErr("warning: could not write .gitignore in the workspace: "+gerr.Error(), outputtools.Yellow)
	}
	return nil
}

// stepCheckCLIRelease (3b) is the last cheap moment: does the CLI release the
// box will download actually exist?
func (i *Implementation) stepCheckCLIRelease(ctx context.Context, st *deployState) error {
	// 3b. Does the CLI release the box will install actually exist?
	//
	// Placed here — after generation, before the first thing that costs money or
	// leaves a trace off this machine — because it is the last cheap moment. The
	// bootstrap downloads that release in its final section, so a missing one fails
	// after the VM, Docker, the database and the app image have all been paid for.
	// Skipped outright when --cli-install-cmd is set — see checkCLIReleaseAsset.
	if err := i.checkCLIReleaseAsset(st.s); err != nil {
		return err
	}
	return nil
}

// stepIssueToken (4 + 5) mints the single-use provisioning token the box pairs
// with, and snapshots the agents that exist now so the new one can be identified
// after pairing.
func (i *Implementation) stepIssueToken(ctx context.Context, st *deployState) error {
	// 4. Mint a single-use provisioning token for headless pairing.
	authCtx, err := productclient.ClientContext()
	if err != nil {
		return fmt.Errorf("building auth context: %w", err)
	}
	st.tokRes, err = i.productClient.ProductClient.IssueProvisioningToken(authCtx, &pb.IssueProvisioningTokenRequest{
		ProjectUuid: st.targets.project.Uuid,
	})
	if err != nil {
		return fmt.Errorf("issuing provisioning token: %w", err)
	}

	// 5. Snapshot existing agents so we can identify the new one after pairing.
	st.existing, err = i.listAgentUUIDs()
	if err != nil {
		return err
	}
	return nil
}
