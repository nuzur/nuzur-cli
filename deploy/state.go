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
}

// SaveDeployment writes (or overwrites) the deployment's state file, 0600.
//
// Timestamps are normalized to UTC HERE rather than at each call site. They used to
// be written in whatever zone the caller happened to build them in — the
// pre-provision record used time.Now().UTC() and the post-provision one time.Now()
// — so two records written eleven minutes apart in one session listed six hours
// apart, with the newer one reading as the older. Ordering was never affected
// (time.Time comparisons are absolute), but "which of these is the stale one" is
// exactly the question that column is read to answer. One zone on disk; converted to
// local once, at print time.
func SaveDeployment(d *Deployment) error {
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
		fmt.Fprintf(os.Stderr,
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
