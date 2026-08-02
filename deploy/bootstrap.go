package deploy

import (
	"bytes"
	_ "embed"
	"fmt"
	"strings"
	"text/template"

	"github.com/nuzur/nuzur-cli/constants"
)

//go:embed templates/bootstrap.sh.tmpl
var bootstrapTemplate string

// BootstrapParams are the values rendered into the remote bootstrap script. The
// DB password is intentionally NOT here — it is generated on the box so the
// plaintext secret never leaves the server.
type BootstrapParams struct {
	Identifier string
	DBEngine   DBEngine
	DBName     string
	DBUser     string
	// DBOnly provisions only the DB engine (--db) + the paired agent + connection
	// (and applies the schema); it skips the generated app, Docker, and Caddy. The
	// database is then managed entirely through nuzur.
	DBOnly bool
	// ExternalDB means the app/agent connect to a caller-supplied existing DB
	// (--db-dsn, local or remote, MySQL or Postgres) instead of a self-hosted one.
	// The bootstrap skips DB install + DB/user creation + backups; DBHost/
	// DBPort/DBPassword/DBParams/DBDSN carry the connection.
	ExternalDB bool
	DBHost     string
	DBPort     string
	DBPassword string // external only; self-hosted generates its own on the box
	DBParams   string // DSN query params (e.g. parseTime=true / sslmode=require)
	DBDSN      string // external only: the raw DSN used for the agent connection
	// DBSchema is the agent connection's default schema — set for Postgres (a
	// namespace like `public`), empty for MySQL where the database is the schema.
	DBSchema    string
	GRPCEnabled bool
	// JWTAuth means the generated app uses the JWT auth server, which reads its
	// signing key from config (auth.jwt.key). The generated base.yaml ships a
	// placeholder, so the bootstrap generates a real random key into prod.yaml —
	// without it token creation is broken.
	JWTAuth bool
	// Domain, when set, makes Caddy serve HTTPS/443 with an automatic Let's
	// Encrypt cert for this project's site. Empty means IP-only: the project gets
	// its own auto-assigned public port on the host IP (plain HTTP), so multiple
	// projects can coexist without domains.
	Domain string
	// Host is the box IP/hostname the CLI connected to; used to compose the
	// IP-only public URL (http://{host}:{publicPort}) written back for the report.
	Host              string
	InnoDBBufferMB    int
	ProjectDir        string // per-project dir, e.g. /etc/nuzur/{identifier} (holds secrets + url)
	ConfigDir         string // per-project config, e.g. /etc/nuzur/{identifier}/config
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
	// CLIVersion PINS which nuzur-cli release the box installs; it defaults to the
	// version of the CLI driving this deploy. Two reasons, both structural:
	//
	//   - box CLI == driving CLI, so the agent on the box is never a different
	//     version from the CLI that paired it and published its connection;
	//   - a release published WHILE a deploy runs can no longer break it. The URL
	//     used to be `releases/latest/download/...`, which resolves at curl time to
	//     whatever Release exists at that instant — and a GitHub Release exists from
	//     the moment it is created, seconds before goreleaser finishes uploading its
	//     assets. Every in-flight deploy 404s during that window, at the very end of
	//     the expensive part (VM, Docker, database and app image all already paid
	//     for). Pinning removes the dependency on what was published minutes ago.
	//
	// A dev/unreleased version still tries the pinned URL: the bootstrap's curl
	// failure names the version AND the exact URL it tried, so the log says plainly
	// that this version has no published assets rather than failing as a generic
	// 404. --cli-install-cmd remains the escape hatch — for boxes that cannot reach
	// GitHub, and for deliberately pinning some other version.
	CLIVersion string
	// NuzurBin is the absolute path to the installed nuzur binary (used in the
	// agent systemd unit).
	NuzurBin string
	// S3* configure the generated app's file-upload endpoints (/upload, /sign).
	// Unlike the DB password, these credentials are resolved from the team's
	// ObjectStore (KMS) and passed IN, then written into prod.yaml (0600) — like
	// the external --db-dsn path, the secret does travel through the bootstrap.
	S3Enabled bool
	S3Region  string
	S3Bucket  string
	S3Key     string
	S3Secret  string
}

// defaults fills unset fields with sensible values.
func (p *BootstrapParams) defaults() {
	if p.InnoDBBufferMB == 0 {
		p.InnoDBBufferMB = 256
	}
	if p.ProjectDir == "" {
		p.ProjectDir = "/etc/nuzur/" + p.Identifier
	}
	if p.ConfigDir == "" {
		p.ConfigDir = p.ProjectDir + "/config"
	}
	if p.NuzurBin == "" {
		p.NuzurBin = "/usr/local/bin/nuzur-cli"
	}
	// Default rather than require: every caller wants "the CLI running this
	// deploy", and defaulting here means direct/test callers cannot accidentally
	// render an unpinned script.
	if p.CLIVersion == "" {
		p.CLIVersion = constants.CLI_VERSION
	}
	// Tolerate a leading `v`: the constant is bare (`1.5.2`) but the git tag and
	// the release URL carry it, and a caller pinning by tag name is the obvious
	// mistake to absorb rather than to render as `.../vv1.5.2/...`.
	p.CLIVersion = strings.TrimPrefix(strings.TrimSpace(p.CLIVersion), "v")
	if p.ImageName == "" {
		p.ImageName = "nuzur/" + p.Identifier + ":latest"
	}
	if p.ContainerName == "" {
		p.ContainerName = p.Identifier + "-api"
	}
	if p.DBParams == "" {
		// Self-hosted DSN query params: MySQL wants parseTime=true; localhost
		// Postgres wants sslmode=disable (no TLS on the loopback socket).
		if p.DBEngine == DBPostgres {
			p.DBParams = "sslmode=disable"
		} else {
			p.DBParams = "parseTime=true"
		}
	}
}

// CLIReleaseArchX8664 is the goreleaser architecture suffix for 64-bit Intel/AMD
// Linux — what every managed provider hands out unless asked otherwise, and the
// asset a pre-flight check probes when it wants to know whether a release was
// published at all (a missing tag 404s every asset under it, whatever the arch).
const CLIReleaseArchX8664 = "x86_64"

// CLIReleaseAssetURL is the GitHub release asset the box downloads the nuzur CLI
// from, for one version and one architecture.
//
// It exists so that URL has ONE definition. The bootstrap template composes the
// same string with the arch resolved on the box (`${NUZUR_ARCH}`), and a caller
// that wants to check the download BEFORE paying for a VM has to ask about
// exactly the URL the script will use — a check aimed one character off is worse
// than no check, because it reports confidently about a different file.
// TestBootstrapTemplateUsesCLIReleaseAssetURL is what keeps the two in step; pass
// "${NUZUR_ARCH}" as the arch to reproduce the template's form.
//
// The leading `v` is added here (the constant is bare, the tag carries it) and a
// caller that passes a tag name is absorbed rather than rendered as `.../vv1.5.2/`.
func CLIReleaseAssetURL(version, arch string) string {
	version = strings.TrimPrefix(strings.TrimSpace(version), "v")
	return fmt.Sprintf("https://github.com/nuzur/nuzur-cli/releases/download/v%s/nuzur-cli_Linux_%s.tar.gz", version, arch)
}

// RenderBootstrap produces the bootstrap shell script for a target.
func RenderBootstrap(p BootstrapParams) (string, error) {
	p.defaults()
	if p.Identifier == "" || p.DBName == "" || p.DBUser == "" {
		return "", fmt.Errorf("bootstrap: Identifier, DBName and DBUser are required")
	}
	if p.RemoteSrcDir == "" && !p.DBOnly {
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
