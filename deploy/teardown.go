package deploy

import (
	"bytes"
	_ "embed"
	"fmt"
	"text/template"
)

//go:embed templates/teardown.sh.tmpl
var teardownTemplate string

// TeardownParams are the values rendered into the remote teardown script. It
// removes THIS project's artifacts (its systemd unit, container, image, its
// /etc/nuzur/{id} config+secrets, its Caddy site snippet, its backup cron, and
// its agent connection). The shared agent, MySQL, and packages are left intact
// unless IsLastProject is set. The database is dropped only when Purge is set.
type TeardownParams struct {
	Identifier    string
	ContainerName string
	ImageName     string
	ProjectDir    string // /etc/nuzur/{identifier}
	DBName        string
	DBUser        string
	ConnUUID      string // this project's agent connection to remove from the shared agent
	NuzurBin      string
	Purge         bool
	// IsLastProject: this is the only project left on the box, so also remove the
	// shared agent (nuzur-agent.service + pairing creds) and the main Caddyfile.
	// When false, the shared agent is kept for the surviving projects.
	IsLastProject bool
}

func (p *TeardownParams) defaults() {
	if p.ImageName == "" {
		p.ImageName = "nuzur/" + p.Identifier + ":latest"
	}
	if p.ContainerName == "" {
		p.ContainerName = p.Identifier + "-api"
	}
	if p.ProjectDir == "" {
		p.ProjectDir = "/etc/nuzur/" + p.Identifier
	}
	if p.NuzurBin == "" {
		p.NuzurBin = "/usr/local/bin/nuzur-cli"
	}
}

// RenderTeardown produces the teardown shell script for a target.
func RenderTeardown(p TeardownParams) (string, error) {
	p.defaults()
	if p.Identifier == "" {
		return "", fmt.Errorf("teardown: Identifier is required")
	}
	if p.Purge && (p.DBName == "" || p.DBUser == "") {
		return "", fmt.Errorf("teardown: DBName and DBUser are required when Purge is set")
	}
	tmpl, err := template.New("teardown").Parse(teardownTemplate)
	if err != nil {
		return "", fmt.Errorf("parsing teardown template: %w", err)
	}
	var buf bytes.Buffer
	if err := tmpl.Execute(&buf, p); err != nil {
		return "", fmt.Errorf("rendering teardown template: %w", err)
	}
	return buf.String(), nil
}
