package deploy

import (
	"bytes"
	_ "embed"
	"fmt"
	"text/template"
)

//go:embed templates/bootstrap.sh.tmpl
var bootstrapTemplate string

// BootstrapParams are the values rendered into the remote bootstrap script. The
// DB password is intentionally NOT here — it is generated on the box so the
// plaintext secret never leaves the server.
type BootstrapParams struct {
	Identifier        string
	DBEngine          DBEngine
	DBName            string
	DBUser            string
	GRPCEnabled       bool
	GRPCPort          string
	HTTPPort          string // always set: REST server or httpServer fallback (auth/info)
	// JWTAuth means the generated app uses the JWT auth server, which reads its
	// signing key from config (auth.jwt.key). The generated base.yaml ships a
	// placeholder, so the bootstrap generates a real random key into prod.yaml —
	// without it token creation is broken.
	JWTAuth bool
	// Domain, when set, makes Caddy serve HTTPS/443 with an automatic Let's
	// Encrypt cert. Empty means IP-only: Caddy serves plain HTTP/80 (no cert), so
	// there are no TLS trust warnings — pass a domain to get real HTTPS.
	Domain string
	InnoDBBufferMB    int
	ConfigDir         string // e.g. /etc/nuzur/config
	RemoteSrcDir      string // where generated source was copied
	ImageName         string
	ContainerName     string
	ProvisioningToken string
	// ConnUUID/ConnName register the localhost DB as a named agent connection
	// (locally, --no-publish) so the daemon serves it by UUID; the deploy
	// command publishes the catalog to nuzur with the user's token.
	ConnUUID string
	ConnName string
	// CLIInstallCmd optionally overrides how the nuzur CLI is installed on the
	// box. When empty, the bootstrap downloads the matching Linux binary from
	// the nuzur-cli GitHub releases. A custom command must leave the binary at
	// NuzurBin.
	CLIInstallCmd string
	// NuzurBin is the absolute path to the installed nuzur binary (used in the
	// agent systemd unit).
	NuzurBin string
}

// defaults fills unset fields with sensible values.
func (p *BootstrapParams) defaults() {
	if p.InnoDBBufferMB == 0 {
		p.InnoDBBufferMB = 256
	}
	if p.ConfigDir == "" {
		p.ConfigDir = "/etc/nuzur/config"
	}
	if p.NuzurBin == "" {
		p.NuzurBin = "/usr/local/bin/nuzur-cli"
	}
	if p.ImageName == "" {
		p.ImageName = "nuzur/" + p.Identifier + ":latest"
	}
	if p.ContainerName == "" {
		p.ContainerName = p.Identifier + "-api"
	}
}

// RenderBootstrap produces the bootstrap shell script for a target.
func RenderBootstrap(p BootstrapParams) (string, error) {
	p.defaults()
	if p.Identifier == "" || p.DBName == "" || p.DBUser == "" {
		return "", fmt.Errorf("bootstrap: Identifier, DBName and DBUser are required")
	}
	if p.RemoteSrcDir == "" {
		return "", fmt.Errorf("bootstrap: RemoteSrcDir is required")
	}
	tmpl, err := template.New("bootstrap").Parse(bootstrapTemplate)
	if err != nil {
		return "", fmt.Errorf("parsing bootstrap template: %w", err)
	}
	var buf bytes.Buffer
	if err := tmpl.Execute(&buf, p); err != nil {
		return "", fmt.Errorf("rendering bootstrap template: %w", err)
	}
	return buf.String(), nil
}
