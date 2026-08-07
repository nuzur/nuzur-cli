package deploy

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/nuzur/nuzur-cli/files"
	"github.com/nuzur/nuzur-cli/outputtools"
)

// Deployment is the persisted record of one `nuzur-cli deploy`, written under
// ~/.config/nuzur/deployments/<id>.json. It is the source of truth for
// `nuzur-cli destroy` (revoke the agent, delete provider infra) and `deploy list`.
type Deployment struct {
	ID                 string   `json:"id"`
	Provider           Provider `json:"provider"`
	ProviderInstanceID string   `json:"provider_instance_id,omitempty"` // cloud VM/instance id (for destroy); empty for BYO-SSH
	// ProviderResourceName is the name nuzur minted for the VM, written to disk
	// BEFORE the provider create call. If a deploy dies during that call, the id
	// never comes back and this name is the only handle left on a VM that may be
	// running and billing — destroy resolves it via Provisioner.FindInstanceByName.
	ProviderResourceName string `json:"provider_resource_name,omitempty"`
	// Provisioning marks a deployment whose VM is still being created. It is set
	// before the create call and cleared once the deploy completes, so a record left
	// with it set is a deploy that died in flight and may have leaked a VM.
	Provisioning       bool     `json:"provisioning,omitempty"`
	Region             string   `json:"region,omitempty"` // cloud region the VM lives in
	// Namespace, ReleaseName, ChartVersion and ImageRef describe a Kubernetes
	// deployment (Provider == ProviderK8s); empty for every other provider.
	// Destroy needs the first two to uninstall the release, and --release-only
	// reads the last two back so it can re-release without regenerating or
	// waiting on CI.
	Namespace    string `json:"namespace,omitempty"`
	ReleaseName  string `json:"release_name,omitempty"`
	ChartVersion string `json:"chart_version,omitempty"`
	ImageRef     string `json:"image_ref,omitempty"`
	Host               string   `json:"host"`
	User               string   `json:"user"`
	Port               int      `json:"port"`
	Identifier         string   `json:"identifier"`
	ProjectUUID        string   `json:"project_uuid"`
	ProjectVersionUUID string   `json:"project_version_uuid"`
	LocalAgentUUID     string   `json:"local_agent_uuid"`
	ConnUUID           string   `json:"conn_uuid,omitempty"`
	DBEngine           DBEngine `json:"db_engine"`
	ExternalDB         bool     `json:"external_db,omitempty"` // --db-dsn: an existing DB, not self-hosted (never dropped on destroy)
	// WorkspaceDir is the persistent app-source WORKSPACE ROOT (e.g.
	// ./nuzur-<identifier>), the directory resolveWorkspace reuses on a
	// re-deploy. Distinct from Spec.SourceDir, which is the app directory
	// INSIDE the workspace (where the Dockerfile lives) — recording that here
	// instead once sent a retried deploy generating into its own app dir. The
	// json key stays source_dir so existing records keep working.
	WorkspaceDir   string    `json:"source_dir,omitempty"`
	Domain         string    `json:"domain,omitempty"`     // set when deployed with --domain (HTTPS site)
	APIURL         string    `json:"api_url,omitempty"`    // resolved front-door URL
	PublicURL      string    `json:"public_url,omitempty"` // same as APIURL; explicit alias
	DataManagerURL string    `json:"data_manager_url,omitempty"`
	CreatedAt      time.Time `json:"created_at"`

	// LastCompletedStep is how far the LAST run of this deployment got: one of the
	// Step* constants below, or "" for a record written before checkpoints existed
	// (or by a run that died before its first checkpoint).
	//
	// It exists because every question the next run asks about a half-finished
	// deploy — did it pair an agent, did it get as far as creating the VM, is this
	// record a live deployment or the debris of one — was previously answered by
	// INFERRING from which fields happened to be empty. An empty LocalAgentUUID
	// meaning "died in flight" is that inference, and it is what turned an
	// interrupted re-deploy into a second record for the same box.
	//
	// The values are self-describing strings rather than an integer, because the
	// file is read by humans debugging exactly this situation.
	//
	// MONOTONE WITHIN A RUN, AND DELIBERATELY NOT ACROSS RUNS. A re-deploy
	// rewrites `finalized` → `box_recorded` → `agent_paired` → `finalized`, so
	// for the middle of every re-deploy this record describes a healthy, serving
	// box as half-deployed. That is the field's definition working — the last run
	// IS the one in progress — but it is worth writing down, because it is the
	// one place a concurrent reader (a second CLI, a crash-recovery path, a human
	// reading the file) can take a true value and draw a false conclusion.
	//
	// Blanking it at the start of a run was considered and rejected. "" is the
	// sentinel for "predates checkpoints", so a re-deploy interrupted after
	// blanking would be indistinguishable from an old record — losing precisely
	// the adoption decision the checkpoint exists to make, which is the bug that
	// used to mint a second record for one box. Reporting less than the truth is
	// not more honest than reporting a truth that has to be read carefully.
	// Telling "in flight" from "got this far" properly needs a second, in-flight
	// marker; it is not a different value of this one.
	//
	// Both directions are pinned: TestDeployRecordSequenceManagedFirstDeploy for
	// within a run, and TestRedeployPreservesURLsMidRun (which observes
	// box_recorded on a record seeded finalized) for across them.
	LastCompletedStep string `json:"last_completed_step,omitempty"`
	// LastError is the error the last run of this deployment ended with, or "" if
	// it finished cleanly. Cleared by the finalizing write, so a record carrying
	// one is a deployment that is currently in a bad state rather than one that
	// once was.
	LastError string `json:"last_error,omitempty"`
}

// The checkpoints a deploy writes into Deployment.LastCompletedStep. Each is
// written by the same mutation that persists the step's result, so the
// checkpoint cannot disagree with the fields it describes.
const (
	// StepPendingRecorded: the record exists and reserves a provider resource
	// NAME, written before the create call so a VM that is created and then lost
	// is still addressable.
	StepPendingRecorded = "pending_recorded"
	// StepInstanceCreated: the provider acknowledged the VM, so the record now
	// carries its instance id. The deploy is still waiting for SSH.
	StepInstanceCreated = "instance_created"
	// StepBoxRecorded: the box exists and is reachable, and the record describes
	// it fully (host, ports, workspace, connection). Everything after this point
	// is software on a server that is already billing.
	StepBoxRecorded = "box_recorded"
	// StepAgentPaired: the local agent for this box is known and registered with
	// nuzur. This is the checkpoint that makes "did the last run get far enough to
	// be reused" a fact rather than a guess.
	StepAgentPaired = "agent_paired"
	// StepReleased: the Helm release is applied and its pods are up
	// (ProviderK8s only). The record now names the release, the chart version
	// and the exact image, which is what makes `--release-only` able to repeat a
	// deploy without regenerating or rebuilding — and what stops the next run
	// re-minting a chart version that has already been published.
	//
	// It is the k8s counterpart of StepAgentPaired — both mark "the workload
	// this deploy exists to run is now running" — but it ranks ABOVE it rather
	// than equal to it. The two are mutually exclusive (each provider skips the
	// other's step), so no record ever carries both, and ranks have to be
	// strictly increasing in pipeline order for "how far did the last run get"
	// to be answerable by comparison alone.
	StepReleased = "released"
	// StepFinalized: the deploy completed its record — agent, front door, data
	// manager link. A schema step may still have failed; that is reported
	// separately and does not make the DEPLOYMENT unfinished.
	StepFinalized = "finalized"
)

// stepRanks orders the checkpoints. Kept next to the constants so a new
// checkpoint cannot be added without deciding where it belongs.
var stepRanks = map[string]int{
	StepPendingRecorded: 1,
	StepInstanceCreated: 2,
	StepBoxRecorded:     3,
	StepAgentPaired:     4,
	StepReleased:        5, // the k8s counterpart of StepAgentPaired; see its comment
	StepFinalized:       6,
}

// StepRank turns a checkpoint into a comparable position, so callers can ask
// "did it get at least as far as X" instead of matching strings.
//
// "" is rank 0 — a record written before checkpoints existed, or by a run that
// died before its first one. An UNRECOGNISED value is also rank 0: the only way
// to see one is a record written by a newer CLI, and the safe reading of a step
// this binary has never heard of is "nothing this binary knows about completed",
// not "further along than anything I know".
func StepRank(step string) int { return stepRanks[step] }

// MutateDeployment applies fn to the deployment's record and writes it back. It
// is the ONLY way the deploy pipeline may write a record.
//
// The point is what it does NOT do: it never replaces the file with a struct the
// caller assembled. Every field fn does not touch keeps the value it had. The
// four write sites in a deploy used to each build a whole Deployment literal,
// which meant each of them had to remember every field of it — and when one
// forgot, the record silently lost the agent uuid, or the resource name, or the
// front-door URLs for the twenty minutes between two writes. Those were three
// separate bugs with one shape.
//
// Load-or-create, and the distinction is load-bearing: a record that is MISSING
// is a first write, but a record that is present and unparseable is a record
// whose contents are unknown — quite possibly the only handle on a running,
// billing VM — so it is an error and is never overwritten with a fresh one.
//
// The returned record is the one that was written, so a caller that needs the
// post-write state does not re-read it.
func MutateDeployment(id string, fn func(*Deployment)) (*Deployment, error) {
	if fn == nil {
		return nil, fmt.Errorf("mutating deployment %q: no mutation given", id)
	}
	rec := &Deployment{}
	data, err := os.ReadFile(files.DeploymentFilePath(id))
	switch {
	case err == nil:
		if uerr := json.Unmarshal(data, rec); uerr != nil {
			return nil, fmt.Errorf("parsing deployment %q: %w", id, uerr)
		}
	case os.IsNotExist(err):
		// First write for this id. Anything else — a permission error, a
		// directory in the way — is a real failure and must not be mistaken for
		// "there was nothing there".
	default:
		return nil, fmt.Errorf("reading deployment %q: %w", id, err)
	}
	fn(rec)
	// The id is the file name, so it is not the mutator's to change: a record
	// whose id disagrees with its path is invisible to `destroy <id>`.
	rec.ID = id
	if err := saveDeployment(rec); err != nil {
		return nil, err
	}
	return rec, nil
}

// SaveDeployment writes a deployment record wholesale.
//
// NOT the production update path — MutateDeployment is, and app/ is checked for
// direct calls to this by TestAppWritesRecordsOnlyThroughMutateDeployment. This
// stays exported as the CREATION path used by tests that seed a machine's record
// store with records that already exist in full.
func SaveDeployment(d *Deployment) error { return saveDeployment(d) }

// saveDeployment writes (or overwrites) the deployment's state file, 0600.
//
// Timestamps are normalized to UTC HERE rather than at each call site. They used to
// be written in whatever zone the caller happened to build them in — the
// pre-provision record used time.Now().UTC() and the post-provision one time.Now()
// — so two records written eleven minutes apart in one session listed six hours
// apart, with the newer one reading as the older. Ordering was never affected
// (time.Time comparisons are absolute), but "which of these is the stale one" is
// exactly the question that column is read to answer. One zone on disk; converted to
// local once, at print time.
func saveDeployment(d *Deployment) error {
	dir := files.DeploymentsDir()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("creating deployments dir: %w", err)
	}
	d.CreatedAt = d.CreatedAt.UTC()
	data, err := json.MarshalIndent(d, "", "  ")
	if err != nil {
		return fmt.Errorf("marshaling deployment: %w", err)
	}
	// Write-to-temp + rename, not an in-place write: this file can be the only
	// handle on a billing VM, and a write interrupted midway used to leave it
	// truncated — which ListDeployments then skipped, hiding the VM from
	// `deploy list` and from the reuse decision. Rename is atomic on the same
	// filesystem, so the record is always either the old version or the new one.
	path := files.DeploymentFilePath(d.ID)
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, append(data, '\n'), 0o600); err != nil {
		return fmt.Errorf("writing deployment state: %w", err)
	}
	if err := os.Rename(tmp, path); err != nil {
		os.Remove(tmp)
		return fmt.Errorf("writing deployment state: %w", err)
	}
	return nil
}

// LoadDeployment reads a single deployment by id.
func LoadDeployment(id string) (*Deployment, error) {
	data, err := os.ReadFile(files.DeploymentFilePath(id))
	if err != nil {
		if os.IsNotExist(err) {
			return nil, fmt.Errorf("no deployment found with id %q", id)
		}
		return nil, err
	}
	var d Deployment
	if err := json.Unmarshal(data, &d); err != nil {
		return nil, fmt.Errorf("parsing deployment %q: %w", id, err)
	}
	return &d, nil
}

// ListDeployments returns all recorded deployments, newest first.
func ListDeployments() ([]Deployment, error) {
	dir := files.DeploymentsDir()
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var out []Deployment
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		data, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err == nil {
			var d Deployment
			if err = json.Unmarshal(data, &d); err == nil {
				out = append(out, d)
				continue
			}
		}
		// A record that cannot be read is not skippable noise: it may be the
		// only handle on a running, billing VM, and silently omitting it makes
		// that VM invisible to `deploy list` and to the deploy reuse decision.
		// Surface it and keep listing the rest.
		fmt.Fprintf(outputtools.Stderr,
			"warning: deployment record %s is unreadable (%v) — it is NOT listed below, but whatever it recorded (possibly a running VM) still exists; inspect or remove the file\n",
			filepath.Join(dir, e.Name()), err)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].CreatedAt.After(out[j].CreatedAt) })
	return out, nil
}

// DeleteDeployment removes a deployment's state file. Not-found is not an error.
func DeleteDeployment(id string) error {
	err := os.Remove(files.DeploymentFilePath(id))
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}
