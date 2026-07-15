package files

import (
	"os"
	"path"
	"path/filepath"
)

// DeploymentsDir returns the persistent per-user directory where the CLI records
// deployments created by `nuzur deploy`, nested under configBaseDir (e.g.
// ~/.config/nuzur/deployments). Each deployment is one JSON file, read by
// `nuzur destroy` / `nuzur deploy list`.
//
// Falls back to /tmp/nuzur-cli/ when UserConfigDir fails, matching the agent
// state helpers.
func DeploymentsDir() string {
	if d, err := os.UserConfigDir(); err == nil {
		return filepath.Join(d, "nuzur", "deployments")
	}
	return path.Join("/tmp", "nuzur-cli", "deployments")
}

// DeploymentFilePath returns the path to a single deployment's state file.
func DeploymentFilePath(id string) string {
	return filepath.Join(DeploymentsDir(), id+".json")
}
