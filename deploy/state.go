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

// Deployment is the persisted record of one `nuzur deploy`, written under
// ~/.config/nuzur/deployments/<id>.json. It is the source of truth for
// `nuzur destroy` (revoke the agent, delete provider infra) and `deploy list`.
type Deployment struct {
	ID                 string    `json:"id"`
	Provider           Provider  `json:"provider"`
	ProviderInstanceID string    `json:"provider_instance_id,omitempty"` // cloud VM/instance id (for destroy); empty for BYO-SSH
	Region             string    `json:"region,omitempty"`               // cloud region the VM lives in
	Host               string    `json:"host"`
	User               string    `json:"user"`
	Port               int       `json:"port"`
	Identifier         string    `json:"identifier"`
	ProjectUUID        string    `json:"project_uuid"`
	ProjectVersionUUID string    `json:"project_version_uuid"`
	LocalAgentUUID     string    `json:"local_agent_uuid"`
	ConnUUID           string    `json:"conn_uuid,omitempty"`
	DBEngine           DBEngine  `json:"db_engine"`
	ExternalDB         bool      `json:"external_db,omitempty"` // --db-dsn: an existing DB, not self-hosted (never dropped on destroy)
	Domain             string    `json:"domain,omitempty"`      // set when deployed with --domain (HTTPS site)
	APIURL             string    `json:"api_url,omitempty"`     // resolved front-door URL
	PublicURL          string    `json:"public_url,omitempty"`  // same as APIURL; explicit alias
	DataManagerURL     string    `json:"data_manager_url,omitempty"`
	CreatedAt          time.Time `json:"created_at"`
}

// SaveDeployment writes (or overwrites) the deployment's state file, 0600.
func SaveDeployment(d *Deployment) error {
	dir := files.DeploymentsDir()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("creating deployments dir: %w", err)
	}
	data, err := json.MarshalIndent(d, "", "  ")
	if err != nil {
		return fmt.Errorf("marshaling deployment: %w", err)
	}
	if err := os.WriteFile(files.DeploymentFilePath(d.ID), append(data, '\n'), 0o600); err != nil {
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
		if err != nil {
			continue
		}
		var d Deployment
		if err := json.Unmarshal(data, &d); err != nil {
			continue
		}
		out = append(out, d)
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
