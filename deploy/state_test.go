package deploy

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
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

// ── MutateDeployment ─────────────────────────────────────────────────────────

// fillEveryField sets every field of a Deployment to a distinctive non-zero
// value, by reflection.
//
// By reflection so that a field ADDED to Deployment is covered the day it is
// added: the preservation test below only proves what it claims if the record it
// starts from has nothing left at its zero value. A field of a kind this does not
// know how to fill fails the test rather than being skipped — a silent skip is
// exactly how a new field would slip through unpreserved.
func fillEveryField(t *testing.T, d *Deployment) {
	t.Helper()
	v := reflect.ValueOf(d).Elem()
	typ := v.Type()
	for i := 0; i < typ.NumField(); i++ {
		field := v.Field(i)
		name := typ.Field(i).Name
		switch {
		case field.Type() == reflect.TypeOf(time.Time{}):
			field.Set(reflect.ValueOf(time.Date(2026, 3, 4, 5, 6, 7, 0, time.UTC)))
		case field.Kind() == reflect.String:
			field.SetString("filled-" + strings.ToLower(name))
		case field.Kind() == reflect.Bool:
			field.SetBool(true)
		case field.Kind() >= reflect.Int && field.Kind() <= reflect.Int64:
			field.SetInt(int64(1000 + i))
		default:
			t.Fatalf("Deployment.%s is a %s, which fillEveryField cannot fill — teach it that kind, "+
				"or this test silently stops covering that field", name, field.Kind())
		}
	}
}

// The property the whole wave rests on: a mutation writes back everything it did
// not touch.
//
// This is round-6 issue 3's direct regression test. That bug was a write site
// assembling a fresh Deployment literal and forgetting one field, which blanked
// the agent uuid on disk for the middle of a re-deploy; the same shape lost the
// provider resource name and the front-door URLs. With a mutator the failure mode
// is not "fixed", it is unavailable — and the reflection walk means a field added
// to the struct next year is covered without anyone remembering to.
func TestMutateDeploymentPreservesUntouchedFields(t *testing.T) {
	isolateDeploymentsDir(t)
	const id = "preserve-1"

	before := &Deployment{}
	fillEveryField(t, before)
	before.ID = id // the id is the file name, not a value under test
	if err := SaveDeployment(before); err != nil {
		t.Fatalf("seeding the record: %v", err)
	}

	// One field touched, deliberately the smallest possible mutation.
	after, err := MutateDeployment(id, func(rec *Deployment) {
		rec.LastCompletedStep = StepBoxRecorded
	})
	if err != nil {
		t.Fatalf("MutateDeployment: %v", err)
	}
	if after.LastCompletedStep != StepBoxRecorded {
		t.Errorf("the mutation did not take: LastCompletedStep = %q", after.LastCompletedStep)
	}

	// Asserted on the record READ BACK FROM DISK, not on the returned struct: the
	// bug being guarded against is what the file ends up containing.
	onDisk, err := LoadDeployment(id)
	if err != nil {
		t.Fatalf("LoadDeployment: %v", err)
	}
	wantV := reflect.ValueOf(*before)
	gotV := reflect.ValueOf(*onDisk)
	typ := wantV.Type()
	for i := 0; i < typ.NumField(); i++ {
		name := typ.Field(i).Name
		if name == "LastCompletedStep" {
			continue // the one field the mutation was allowed to change
		}
		want, got := wantV.Field(i).Interface(), gotV.Field(i).Interface()
		if wantTime, ok := want.(time.Time); ok {
			if !wantTime.Equal(got.(time.Time)) {
				t.Errorf("Deployment.%s = %v after an unrelated mutation, want %v", name, got, want)
			}
			continue
		}
		if !reflect.DeepEqual(want, got) {
			t.Errorf("Deployment.%s = %#v after an unrelated mutation, want %#v", name, got, want)
		}
	}
}

// A missing record is a first write. Anything else that stops the record being
// read is a failure — never a reason to start a new one on top of it.
func TestMutateDeploymentCreatesOnlyOnNotExist(t *testing.T) {
	isolateDeploymentsDir(t)

	// Not there: created, with the id the caller asked for.
	created, err := MutateDeployment("fresh-1", func(rec *Deployment) {
		rec.Identifier = "sfapi"
		rec.LastCompletedStep = StepPendingRecorded
	})
	if err != nil {
		t.Fatalf("MutateDeployment on a missing record: %v", err)
	}
	if created.ID != "fresh-1" || created.Identifier != "sfapi" {
		t.Errorf("created record = %+v, want id fresh-1 and identifier sfapi", created)
	}
	back, err := LoadDeployment("fresh-1")
	if err != nil {
		t.Fatalf("the created record is not readable: %v", err)
	}
	if back.LastCompletedStep != StepPendingRecorded {
		t.Errorf("on-disk LastCompletedStep = %q, want %q", back.LastCompletedStep, StepPendingRecorded)
	}

	// Unreadable for a reason that is NOT absence — here a directory where the
	// record should be, which is the cheapest portable stand-in for a permission
	// error. Absence is the only condition that may start a new record.
	if err := os.MkdirAll(files.DeploymentFilePath("blocked-1"), 0o700); err != nil {
		t.Fatalf("staging the unreadable record: %v", err)
	}
	if _, err := MutateDeployment("blocked-1", func(rec *Deployment) {
		rec.Identifier = "should-not-be-written"
	}); err == nil {
		t.Fatal("MutateDeployment treated an unreadable record as a missing one")
	}
	if info, err := os.Stat(files.DeploymentFilePath("blocked-1")); err != nil || !info.IsDir() {
		t.Errorf("the unreadable path was replaced: err=%v", err)
	}
}

// A record that cannot be parsed may be the only handle on a running, billing VM.
// Overwriting it with a fresh one — which is what load-or-create would do if it
// treated "unparseable" as "absent" — destroys the evidence and the handle.
func TestMutateDeploymentRefusesCorruptRecord(t *testing.T) {
	isolateDeploymentsDir(t)
	// The exact shape an interrupted in-place write left before writes became
	// atomic, and the shape a half-copied backup still has.
	corrupt := []byte(`{"id":"corrupt-1","provider_instance_id":"do-99","host":`)
	path := files.DeploymentFilePath("corrupt-1")
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, corrupt, 0o600); err != nil {
		t.Fatal(err)
	}

	if _, err := MutateDeployment("corrupt-1", func(rec *Deployment) {
		rec.LastCompletedStep = StepFinalized
	}); err == nil {
		t.Fatal("MutateDeployment overwrote an unparseable record instead of refusing it")
	}

	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading the record back: %v", err)
	}
	if string(got) != string(corrupt) {
		t.Errorf("the corrupt record was rewritten:\n got %q\nwant %q", got, corrupt)
	}
}

// Records written before checkpoints existed keep working, and read as
// "nothing known completed" rather than as anything else.
//
// This is the whole migration story: there is no migration. The fields are
// omitempty strings, an absent one unmarshals to "", and StepRank("") is 0.
func TestOldRecordWithoutCheckpointFieldsLoads(t *testing.T) {
	isolateDeploymentsDir(t)
	// A record exactly as the pre-checkpoint CLI wrote one.
	old := `{
  "id": "sfapi-c3d31228",
  "provider": "digitalocean",
  "provider_instance_id": "do-501",
  "provider_resource_name": "nuzur-sfapi-01",
  "region": "nyc3",
  "host": "203.0.113.10",
  "user": "root",
  "port": 22,
  "identifier": "sfapi",
  "project_uuid": "f8888e33-0000-0000-0000-0000000000a1",
  "project_version_uuid": "f8888e33-0000-0000-0000-0000000000a2",
  "local_agent_uuid": "f8888e33-0000-0000-0000-000000000001",
  "conn_uuid": "f8888e33-0000-0000-0000-0000000000c1",
  "db_engine": "mysql",
  "source_dir": "/w/nuzur-sfapi",
  "api_url": "http://203.0.113.10:8443",
  "public_url": "http://203.0.113.10:8443",
  "created_at": "2026-07-01T09:30:00Z"
}
`
	path := files.DeploymentFilePath("sfapi-c3d31228")
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(old), 0o600); err != nil {
		t.Fatal(err)
	}

	rec, err := LoadDeployment("sfapi-c3d31228")
	if err != nil {
		t.Fatalf("LoadDeployment on a pre-checkpoint record: %v", err)
	}
	if rec.LastCompletedStep != "" || rec.LastError != "" {
		t.Errorf("checkpoint fields invented on an old record: step=%q err=%q", rec.LastCompletedStep, rec.LastError)
	}
	if StepRank(rec.LastCompletedStep) != 0 {
		t.Errorf("StepRank of an old record = %d, want 0", StepRank(rec.LastCompletedStep))
	}
	if rec.Host != "203.0.113.10" || rec.LocalAgentUUID == "" || rec.WorkspaceDir != "/w/nuzur-sfapi" {
		t.Errorf("an old record did not round-trip: %+v", rec)
	}
	// And it is still listable — an unreadable record is warned about and omitted,
	// which for a record describing a live VM would be the serious failure.
	list, err := ListDeployments()
	if err != nil || len(list) != 1 {
		t.Fatalf("ListDeployments = %v, %v; want the one old record", list, err)
	}

	// Mutating it adopts the checkpoint without disturbing anything else.
	after, err := MutateDeployment("sfapi-c3d31228", func(r *Deployment) {
		r.LastCompletedStep = StepFinalized
	})
	if err != nil {
		t.Fatalf("MutateDeployment on an old record: %v", err)
	}
	if after.LocalAgentUUID != rec.LocalAgentUUID || after.APIURL != rec.APIURL || !after.CreatedAt.Equal(rec.CreatedAt) {
		t.Errorf("upgrading an old record changed it: %+v", after)
	}
}

// The checkpoints are ordered, and the order is the deploy's order. Callers
// compare rank rather than string, so a checkpoint added out of position would
// otherwise turn "did it get as far as pairing" into a quietly wrong answer.
func TestStepRankOrdersCheckpoints(t *testing.T) {
	order := []string{StepPendingRecorded, StepInstanceCreated, StepBoxRecorded, StepAgentPaired, StepFinalized}
	for i := 1; i < len(order); i++ {
		if StepRank(order[i]) <= StepRank(order[i-1]) {
			t.Errorf("StepRank(%s)=%d is not after StepRank(%s)=%d",
				order[i], StepRank(order[i]), order[i-1], StepRank(order[i-1]))
		}
	}
	if StepRank("") != 0 {
		t.Errorf(`StepRank("") = %d, want 0 — a pre-checkpoint record completed nothing we know of`, StepRank(""))
	}
	// A step written by a NEWER CLI. Rank 0 is the conservative reading: this
	// binary knows of nothing that completed, rather than claiming it got further
	// than any step this binary has.
	if StepRank("some_future_step") != 0 {
		t.Errorf("StepRank of an unknown step = %d, want 0", StepRank("some_future_step"))
	}
}
