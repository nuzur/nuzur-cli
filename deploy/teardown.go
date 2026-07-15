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
// removes the nuzur-installed artifacts (services, container, image, config,
// secrets, agent pairing, Caddy site, backup cron); the box and shared packages
// are left intact. The database is dropped only when Purge is set.
type TeardownParams struct {
	Identifier    string
	ContainerName string
	ImageName     string
	DBName        string
	DBUser        string
	Purge         bool
}

func (p *TeardownParams) defaults() {
	if p.ImageName == "" {
		p.ImageName = "nuzur/" + p.Identifier + ":latest"
	}
	if p.ContainerName == "" {
		p.ContainerName = p.Identifier + "-api"
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
