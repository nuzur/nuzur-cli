package deploy

import (
	"strings"
	"testing"
)

// The `aws:` block the bootstrap writes into prod.yaml now serves two providers:
// AWS S3, which needs no endpoint (the SDK derives one from the region), and any
// S3-COMPATIBLE store — Cloudflare R2 — which needs an explicit one.
//
// The rule that makes that safe is that an empty endpoint emits NOTHING. The
// generated app's base.yaml defaults `endpoint` to blank and go.uber.org/config
// merges base under prod, so a key prod.yaml never writes keeps the blank
// default — which is exactly the pre-R2 behaviour. If the template emitted
// `endpoint:` unconditionally, every existing AWS deploy's config would change
// shape on its next re-deploy, and an empty value would have to mean "AWS" by
// convention in the app rather than by absence.

func s3BootstrapParams(endpoint string) BootstrapParams {
	return BootstrapParams{
		Identifier:        "shop",
		DBEngine:          DBMySQL,
		DBName:            "shop",
		DBUser:            "shop_app",
		Host:              "1.2.3.4",
		RemoteSrcDir:      "/opt/nuzur/shop/src",
		ImageName:         "nuzur/shop:test",
		ProvisioningToken: "nzpt_test",
		ConnUUID:          "conn-uuid-1",
		ConnName:          "shop-db",
		S3Enabled:         true,
		S3Region:          "us-east-1",
		S3Bucket:          "app-uploads",
		S3Key:             "AKIAEXAMPLE",
		S3Secret:          "s3-secret",
		S3Endpoint:        endpoint,
	}
}

func renderOrFail(t *testing.T, p BootstrapParams) string {
	t.Helper()
	script, err := RenderBootstrap(p)
	if err != nil {
		t.Fatalf("RenderBootstrap: %v", err)
	}
	return script
}

// awsBlock extracts the `aws:` block from the prod.yaml heredoc, so the
// assertions are about the config the app reads and not about coincidental
// matches elsewhere in a 23KB shell script.
func awsBlock(t *testing.T, script string) string {
	t.Helper()
	idx := strings.Index(script, "aws:\n")
	if idx < 0 {
		t.Fatalf("rendered script has no aws: block")
	}
	rest := script[idx:]
	end := strings.Index(rest, "YAML\n")
	if end < 0 {
		t.Fatalf("aws: block is not inside the prod.yaml heredoc")
	}
	return rest[:end]
}

// An AWS S3 deploy must render exactly what it rendered before R2 existed: no
// endpoint line in the config and no endpoint variable in the shell.
func TestBootstrapS3OmitsEndpoint(t *testing.T) {
	script := renderOrFail(t, s3BootstrapParams(""))
	if strings.Contains(script, "S3_ENDPOINT") {
		t.Errorf("an AWS S3 bootstrap set S3_ENDPOINT; it must emit nothing at all:\n%s", awsBlock(t, script))
	}
	if got := awsBlock(t, script); strings.Contains(got, "endpoint") {
		t.Errorf("an AWS S3 aws: block carries an endpoint key:\n%s", got)
	}
}

// Cloudflare R2: the endpoint reaches prod.yaml, through a single-quoted shell
// variable like every other credential (so an endpoint with shell metacharacters
// cannot be interpolated by the box's shell).
func TestBootstrapR2EmitsEndpoint(t *testing.T) {
	const endpoint = "https://acct123.r2.cloudflarestorage.com"
	script := renderOrFail(t, s3BootstrapParams(endpoint))

	if want := "S3_ENDPOINT='" + endpoint + "'\n"; !strings.Contains(script, want) {
		t.Errorf("rendered script does not assign %s", strings.TrimSpace(want))
	}
	block := awsBlock(t, script)
	if !strings.Contains(block, "  endpoint: ${S3_ENDPOINT}\n") {
		t.Errorf("aws: block does not read the endpoint variable:\n%s", block)
	}
	// The endpoint is written LAST so the four pre-existing keys keep their order
	// — a diff of two prod.yaml files should show one added line, not a reshuffle.
	if !strings.HasSuffix(block, "  bucket: ${S3_BUCKET}\n  endpoint: ${S3_ENDPOINT}\n") {
		t.Errorf("aws: block does not end with bucket then endpoint:\n%s", block)
	}
}

// The two renderings differ by exactly the two lines the endpoint adds, and
// nothing else — the guard that a future edit inside the {{if .S3Enabled}} block
// cannot quietly change what an AWS deploy ships.
func TestBootstrapR2DiffersFromS3ByEndpointLinesOnly(t *testing.T) {
	const endpoint = "https://acct123.r2.cloudflarestorage.com"
	s3Lines := strings.Split(renderOrFail(t, s3BootstrapParams("")), "\n")
	r2Lines := strings.Split(renderOrFail(t, s3BootstrapParams(endpoint)), "\n")

	var added []string
	i, j := 0, 0
	for i < len(s3Lines) && j < len(r2Lines) {
		if s3Lines[i] == r2Lines[j] {
			i++
			j++
			continue
		}
		added = append(added, r2Lines[j])
		j++
	}
	added = append(added, r2Lines[j:]...)
	if i != len(s3Lines) {
		t.Fatalf("the R2 rendering dropped or reordered lines from the S3 one (stopped at S3 line %d: %q)", i, s3Lines[i])
	}
	want := []string{"S3_ENDPOINT='" + endpoint + "'", "  endpoint: ${S3_ENDPOINT}"}
	if len(added) != len(want) {
		t.Fatalf("R2 added %d lines, want exactly %d:\n%q", len(added), len(want), added)
	}
	for k := range want {
		if added[k] != want[k] {
			t.Errorf("added line %d = %q, want %q", k, added[k], want[k])
		}
	}
}
