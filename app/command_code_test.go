package app

import (
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"io"
	"strings"
	"testing"

	nemgen "github.com/nuzur/nem/idl/gen"
	"github.com/nuzur/nuzur-cli/codescan"
	"github.com/nuzur/nuzur-cli/productclient"
	pb "github.com/nuzur/nuzur-cli/protodeps/gen"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/encoding/protojson"
)

// codeFakeClient scripts the code-snapshot RPCs; anything else panics (nil
// embedded interface), which is the point: upload must call nothing else.
type codeFakeClient struct {
	pb.NuzurProductClient
	existing []*pb.CodeSnapshotItem
	uploads  []*pb.UploadCodeSnapshotRequest
	archived []string
	// scanned is the set of commits GetCodeSnapshot finds.
	scanned map[string]bool
}

func (f *codeFakeClient) ListCodeSnapshots(ctx context.Context, in *pb.ListCodeSnapshotsRequest, opts ...grpc.CallOption) (*pb.ListCodeSnapshotsResponse, error) {
	return &pb.ListCodeSnapshotsResponse{Snapshots: f.existing}, nil
}

func (f *codeFakeClient) UploadCodeSnapshot(ctx context.Context, in *pb.UploadCodeSnapshotRequest, opts ...grpc.CallOption) (*pb.UploadCodeSnapshotResponse, error) {
	f.uploads = append(f.uploads, in)
	return &pb.UploadCodeSnapshotResponse{
		Snapshot: &nemgen.CodeSnapshot{CommitSha: "aef8d2d6c1f3b6f7e0c9d8a7b6c5d4e3f2a1b0c9"},
		Summary:  &pb.CodeSummary{Findings: []*pb.CodeFinding{{Severity: "warning", Title: "3 components depend on each other in a cycle"}}},
	}, nil
}

func (f *codeFakeClient) GetCodeSnapshot(ctx context.Context, in *pb.GetCodeSnapshotRequest, opts ...grpc.CallOption) (*pb.GetCodeSnapshotResponse, error) {
	if !f.scanned[in.GetCommitSha()] {
		return nil, status.Error(codes.NotFound, "code snapshot not found")
	}
	return &pb.GetCodeSnapshotResponse{Snapshot: &nemgen.CodeSnapshot{CommitSha: in.GetCommitSha()}}, nil
}

func (f *codeFakeClient) ArchiveCodebase(ctx context.Context, in *pb.ArchiveCodebaseRequest, opts ...grpc.CallOption) (*nemgen.Codebase, error) {
	f.archived = append(f.archived, in.GetCodebaseUuid())
	return &nemgen.Codebase{Uuid: in.GetCodebaseUuid()}, nil
}

func codeTestFacts() *pb.CodeFacts {
	return &pb.CodeFacts{
		SchemaVersion: 1, ScannerVersion: "0.1.0", ModulePath: "example.com/app",
		Git: &pb.CodeGitInfo{CommitSha: "aef8d2d6c1f3b6f7e0c9d8a7b6c5d4e3f2a1b0c9", RemoteUrl: "https://github.com/org/app"},
		Nodes: []*pb.CodeNode{
			{Id: "ext:redis", Kind: pb.CodeNodeKind_CODE_NODE_KIND_EXTERNAL_SYSTEM, Name: "redis"},
			{Id: "entity:season", Kind: pb.CodeNodeKind_CODE_NODE_KIND_ENTITY_REF, Name: "season"},
		},
		Routes: []*pb.CodeRoute{{Method: "GET", Pattern: "/season/{id}"}},
	}
}

func TestCodeUploadFirstNeedsConsent(t *testing.T) {
	isolateHome(t)
	fake := &codeFakeClient{}
	i := &Implementation{productClient: &productclient.Client{ProductClient: fake}}
	var out bytes.Buffer
	link := codescan.Link{CodebaseUUID: "cb", Identifier: "app"}

	// Tests run without a terminal: the first upload must stop at the preview.
	err := i.codeUpload(link, codeTestFacts(), false, strings.NewReader(""), &out)
	if err == nil || !strings.Contains(err.Error(), "--yes") {
		t.Fatalf("err = %v, want a request for --yes", err)
	}
	if len(fake.uploads) != 0 {
		t.Fatal("uploaded without consent")
	}
	for _, want := range []string{"no source code", "GET /season/{id}", "redis", "season"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("preview misses %q:\n%s", want, out.String())
		}
	}
}

func TestCodeUploadSendsGzipFacts(t *testing.T) {
	isolateHome(t)
	fake := &codeFakeClient{}
	i := &Implementation{productClient: &productclient.Client{ProductClient: fake}}
	var out bytes.Buffer
	if err := i.codeUpload(codescan.Link{CodebaseUUID: "cb", Identifier: "app"}, codeTestFacts(), true, strings.NewReader(""), &out); err != nil {
		t.Fatal(err)
	}
	if len(fake.uploads) != 1 || fake.uploads[0].GetCodebaseUuid() != "cb" {
		t.Fatalf("uploads: %+v", fake.uploads)
	}
	zr, err := gzip.NewReader(bytes.NewReader(fake.uploads[0].GetFactsGzip()))
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := io.ReadAll(zr)
	got := &pb.CodeFacts{}
	if err := protojson.Unmarshal(raw, got); err != nil || got.GetModulePath() != "example.com/app" {
		t.Fatalf("body is not the facts: %v", err)
	}
	if !strings.Contains(out.String(), "Uploaded aef8d2d") || !strings.Contains(out.String(), "in a cycle") {
		t.Fatalf("output:\n%s", out.String())
	}
}

func TestCodeUploadLaterNeedsNoConsent(t *testing.T) {
	isolateHome(t)
	fake := &codeFakeClient{existing: []*pb.CodeSnapshotItem{{}}}
	i := &Implementation{productClient: &productclient.Client{ProductClient: fake}}
	if err := i.codeUpload(codescan.Link{CodebaseUUID: "cb", Identifier: "app"}, codeTestFacts(), false, strings.NewReader(""), io.Discard); err != nil {
		t.Fatal(err)
	}
	if len(fake.uploads) != 1 {
		t.Fatal("a codebase with snapshots should upload without asking")
	}
}

func TestAskYesNo(t *testing.T) {
	for in, want := range map[string]bool{"y\n": true, "YES\n": true, "n\n": false, "\n": false, "maybe\n": false} {
		got, err := askYesNo(strings.NewReader(in), io.Discard, "?")
		if err != nil || got != want {
			t.Errorf("%q: got %v, %v", in, got, err)
		}
	}
	if _, err := askYesNo(strings.NewReader(""), io.Discard, "?"); err == nil {
		t.Error("EOF without an answer must be an error, not a silent no")
	}
}

func TestCodeArchive(t *testing.T) {
	isolateHome(t)
	fake := &codeFakeClient{}
	i := &Implementation{productClient: &productclient.Client{ProductClient: fake}}
	link := codescan.Link{CodebaseUUID: "cb", Identifier: "app"}

	// Outside a terminal, archiving needs --yes and must not call the server.
	if err := i.codeArchive(link, false, strings.NewReader(""), io.Discard); err == nil || !strings.Contains(err.Error(), "--yes") {
		t.Fatalf("err = %v, want a request for --yes", err)
	}
	if len(fake.archived) != 0 {
		t.Fatal("archived without confirmation")
	}
	var out bytes.Buffer
	if err := i.codeArchive(link, true, strings.NewReader(""), &out); err != nil {
		t.Fatal(err)
	}
	if len(fake.archived) != 1 || fake.archived[0] != "cb" || !strings.Contains(out.String(), "Archived codebase app") {
		t.Fatalf("archived %v, output %q", fake.archived, out.String())
	}
}

func TestCodeScanBranchPoint(t *testing.T) {
	isolateHome(t)
	const bp = "1111111111111111111111111111111111111111"
	link := codescan.Link{CodebaseUUID: "cb", Identifier: "app"}
	onBranch := func() *pb.CodeFacts {
		f := codeTestFacts()
		f.Git.Branch, f.Git.BranchPointSha, f.Git.DefaultBranch = "release/2026-11", bp, "master"
		return f
	}

	// Not scanned yet: scan it (in a worktree, here faked) and upload it.
	fake := &codeFakeClient{existing: []*pb.CodeSnapshotItem{{}}}
	i := &Implementation{productClient: &productclient.Client{ProductClient: fake}}
	var scannedAt []string
	scan := func(sha string) (*pb.CodeFacts, error) {
		scannedAt = append(scannedAt, sha)
		f := codeTestFacts()
		f.Git.CommitSha = sha
		return f, nil
	}
	var out bytes.Buffer
	if err := i.codeScanBranchPoint(link, onBranch(), scan, &out); err != nil {
		t.Fatal(err)
	}
	if len(scannedAt) != 1 || scannedAt[0] != bp || len(fake.uploads) != 1 {
		t.Fatalf("expected one scan and upload of %s, got scans %v, %d uploads", bp, scannedAt, len(fake.uploads))
	}
	if !strings.Contains(out.String(), "where release/2026-11 split from master") {
		t.Fatalf("output should say what is being scanned: %q", out.String())
	}

	// Already scanned, or on the default branch: nothing to do.
	for name, facts := range map[string]*pb.CodeFacts{"scanned": onBranch(), "default branch": codeTestFacts()} {
		fake := &codeFakeClient{scanned: map[string]bool{bp: true}}
		i := &Implementation{productClient: &productclient.Client{ProductClient: fake}}
		scannedAt = nil
		if err := i.codeScanBranchPoint(link, facts, scan, io.Discard); err != nil || len(scannedAt) != 0 || len(fake.uploads) != 0 {
			t.Fatalf("%s: expected no scan, got %v, %d uploads, %v", name, scannedAt, len(fake.uploads), err)
		}
	}

	// A failing branch-point scan is reported, not fatal: HEAD is uploaded.
	fake = &codeFakeClient{existing: []*pb.CodeSnapshotItem{{}}}
	i = &Implementation{productClient: &productclient.Client{ProductClient: fake}}
	out.Reset()
	fail := func(string) (*pb.CodeFacts, error) { return nil, errors.New("go.mod missing") }
	if err := i.codeScanBranchPoint(link, onBranch(), fail, &out); err != nil {
		t.Fatalf("should not fail the scan: %v", err)
	}
	if !strings.Contains(out.String(), "Could not scan the branch point") {
		t.Fatalf("the failure should be reported: %q", out.String())
	}
}
