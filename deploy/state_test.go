package deploy

import (
	"encoding/json"
	"os"
	"testing"
	"time"

	"github.com/nuzur/nuzur-cli/files"
)

// isolateDeploymentsDir points files.DeploymentsDir() at a temp directory for the
// duration of a test. os.UserConfigDir reads HOME on darwin and XDG_CONFIG_HOME on
// unix, so both are set.
func isolateDeploymentsDir(t *testing.T) {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("HOME", dir)
	t.Setenv("XDG_CONFIG_HOME", dir)
	if got := files.DeploymentsDir(); !osPathUnder(got, dir) {
		t.Skipf("cannot isolate the deployments dir on this platform (got %s)", got)
	}
}

func osPathUnder(path, dir string) bool {
	return len(path) > len(dir) && path[:len(dir)] == dir
}

// Every record timestamp is written in UTC, whatever zone the caller built it in.
//
// They were not: the pre-provision record used time.Now().UTC() and the record
// written once the box existed used time.Now(). Two records eleven minutes apart in
// one session listed as `18:09` and `11:58` — a six-hour phantom gap, with the NEWER
// one reading as the older, while it was still mid-provision. Ordering was never
// affected (time.Time comparisons are absolute), but "which of these is the stale
// one" is exactly the question that column is read to answer.
func TestSaveDeploymentNormalizesTimestampsToUTC(t *testing.T) {
	isolateDeploymentsDir(t)

	// A zone that is neither UTC nor, on most machines, local — so a record built
	// with time.Now() in any zone lands the same way on disk.
	kolkata := time.FixedZone("IST", 5*3600+1800)
	created := time.Date(2026, 8, 1, 18, 9, 0, 0, kolkata)

	d := &Deployment{ID: "r6box-c3d31228", Host: "64.225.62.77", Identifier: "r6box", CreatedAt: created}
	if err := SaveDeployment(d); err != nil {
		t.Fatalf("SaveDeployment: %v", err)
	}

	// Normalized on the struct too, so a caller that goes on using it (deploy writes
	// the same record twice) cannot reintroduce the drift.
	if d.CreatedAt.Location() != time.UTC {
		t.Errorf("in-memory CreatedAt is in %v, want UTC", d.CreatedAt.Location())
	}
	if !d.CreatedAt.Equal(created) {
		t.Errorf("normalizing changed the instant: %v != %v", d.CreatedAt, created)
	}

	raw, err := os.ReadFile(files.DeploymentFilePath(d.ID))
	if err != nil {
		t.Fatalf("reading the record: %v", err)
	}
	var onDisk struct {
		CreatedAt string `json:"created_at"`
	}
	if err := json.Unmarshal(raw, &onDisk); err != nil {
		t.Fatalf("parsing the record: %v", err)
	}
	// RFC3339 with a Z, not a +05:30 offset: one zone on disk means two records can
	// be compared by reading them.
	if want := "2026-08-01T12:39:00Z"; onDisk.CreatedAt != want {
		t.Errorf("created_at on disk = %q, want %q", onDisk.CreatedAt, want)
	}

	back, err := LoadDeployment(d.ID)
	if err != nil {
		t.Fatalf("LoadDeployment: %v", err)
	}
	if !back.CreatedAt.Equal(created) {
		t.Errorf("round-tripped CreatedAt = %v, want the same instant as %v", back.CreatedAt, created)
	}
}

// A record that cannot be parsed must not silently vanish from the listing —
// it may be the only handle on a billing VM. It is warned about (stderr) and
// the healthy records still list.
func TestListDeploymentsSurfacesCorruptRecords(t *testing.T) {
	isolateDeploymentsDir(t)
	good := &Deployment{ID: "good-1", Identifier: "app", Host: "1.2.3.4", CreatedAt: time.Now()}
	if err := SaveDeployment(good); err != nil {
		t.Fatal(err)
	}
	// A truncated write, the exact shape an interrupted in-place WriteFile left.
	if err := os.WriteFile(files.DeploymentFilePath("bad-1"), []byte(`{"id":"bad-1","host":`), 0o600); err != nil {
		t.Fatal(err)
	}
	out, err := ListDeployments()
	if err != nil {
		t.Fatalf("ListDeployments: %v", err)
	}
	if len(out) != 1 || out[0].ID != "good-1" {
		t.Fatalf("expected the one healthy record, got %+v", out)
	}
}

// SaveDeployment goes through a temp file + rename so an interrupted write can
// never truncate the live record; no .tmp residue is left behind on success.
func TestSaveDeploymentIsAtomicAndLeavesNoTempFile(t *testing.T) {
	isolateDeploymentsDir(t)
	d := &Deployment{ID: "atomic-1", Identifier: "app", WorkspaceDir: "/w/nuzur-app", CreatedAt: time.Now()}
	if err := SaveDeployment(d); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(files.DeploymentFilePath("atomic-1") + ".tmp"); !os.IsNotExist(err) {
		t.Fatalf("temp file left behind: %v", err)
	}
	got, err := LoadDeployment("atomic-1")
	if err != nil {
		t.Fatal(err)
	}
	if got.WorkspaceDir != "/w/nuzur-app" {
		t.Fatalf("WorkspaceDir did not round-trip: %q", got.WorkspaceDir)
	}
	// The rename to WorkspaceDir must not orphan existing records: the json key
	// is still source_dir.
	raw, err := os.ReadFile(files.DeploymentFilePath("atomic-1"))
	if err != nil {
		t.Fatal(err)
	}
	var onDisk map[string]any
	if err := json.Unmarshal(raw, &onDisk); err != nil {
		t.Fatal(err)
	}
	if onDisk["source_dir"] != "/w/nuzur-app" {
		t.Fatalf("on-disk json key changed: %v", onDisk)
	}
}
