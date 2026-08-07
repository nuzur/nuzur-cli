package deploy

import (
	"context"
	"strings"
	"testing"
	"time"
)

// stubLocal replaces LocalCommand for the duration of a test. Any command the
// stub does not recognise fails loudly rather than returning an empty string,
// which would otherwise read as a legitimate "no runs yet".
func stubLocal(t *testing.T, fn func(dir, name string, args []string) (string, error)) {
	t.Helper()
	prev := LocalCommand
	prevLook := LookLocal
	LocalCommand = func(ctx context.Context, dir, name string, args ...string) (string, error) {
		return fn(dir, name, args)
	}
	LookLocal = func(string) error { return nil }
	t.Cleanup(func() { LocalCommand = prev; LookLocal = prevLook })
}

// TestImageTagForSHA pins the tag format against the workflow that produces it.
// docker/metadata-action's type=sha,format=long emits the FULL sha; the short
// form differs only in a way that makes every pull 404.
func TestImageTagForSHA(t *testing.T) {
	sha := "a1b2c3d4e5f60718293a4b5c6d7e8f9012345678"
	if got, want := ImageTagForSHA(sha), "sha-"+sha; got != want {
		t.Errorf("ImageTagForSHA = %q, want %q", got, want)
	}
	if got := ImageTagForSHA(sha); len(strings.TrimPrefix(got, "sha-")) != 40 {
		t.Errorf("tag must carry the full 40-char sha, got %q", got)
	}
}

func TestWaitForImageBuildSucceeds(t *testing.T) {
	calls := 0
	stubLocal(t, func(dir, name string, args []string) (string, error) {
		calls++
		switch calls {
		case 1:
			return `[]`, nil // GitHub has not registered the run yet
		case 2:
			return `[{"databaseId":1,"status":"in_progress","conclusion":"","headSha":"abc"}]`, nil
		default:
			return `[{"databaseId":1,"status":"completed","conclusion":"success","headSha":"abc"}]`, nil
		}
	})

	var states []string
	err := WaitForImageBuild(context.Background(), CIWaitOptions{
		SHA: "abc", Workflow: "publish-myapp-image.yaml",
		Poll: time.Millisecond, Timeout: time.Second,
		OnProgress: func(s string) { states = append(states, s) },
	})
	if err != nil {
		t.Fatalf("WaitForImageBuild: %v", err)
	}
	// The empty first response must NOT be treated as a failure: a run does not
	// exist the instant a push lands.
	if len(states) < 2 {
		t.Errorf("expected progress through several states, got %v", states)
	}
}

// TestWaitForImageBuildReportsFailure: a red build must stop the deploy and
// point at the log, not fall through to releasing a stale image.
func TestWaitForImageBuildReportsFailure(t *testing.T) {
	stubLocal(t, func(dir, name string, args []string) (string, error) {
		return `[{"databaseId":42,"status":"completed","conclusion":"failure","headSha":"abcdef1234"}]`, nil
	})
	err := WaitForImageBuild(context.Background(), CIWaitOptions{
		SHA: "abcdef1234", Workflow: "w.yaml", Poll: time.Millisecond, Timeout: time.Second,
	})
	if err == nil {
		t.Fatal("expected a failed CI build to fail the deploy")
	}
	for _, want := range []string{"failure", "gh run view 42"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error should mention %q, got: %v", want, err)
		}
	}
}

// TestWaitForImageBuildTimesOut: the message has to leave the user somewhere to
// go, since the build may simply be slow rather than broken.
func TestWaitForImageBuildTimesOut(t *testing.T) {
	stubLocal(t, func(dir, name string, args []string) (string, error) {
		return `[{"databaseId":1,"status":"in_progress","conclusion":"","headSha":"abc"}]`, nil
	})
	err := WaitForImageBuild(context.Background(), CIWaitOptions{
		SHA: "abc", Workflow: "w.yaml", Poll: time.Millisecond, Timeout: 5 * time.Millisecond,
	})
	if err == nil {
		t.Fatal("expected a timeout")
	}
	if !strings.Contains(err.Error(), "--no-wait") {
		t.Errorf("timeout should suggest a way forward, got: %v", err)
	}
}

// TestWaitForImageBuildMatchesTheCommitClientSide is a regression test for a
// real deploy failure.
//
// `gh run list --commit` only exists in gh 2.42+. On older versions it is not
// ignored — it is a hard `unknown flag` error, which killed the wait on a build
// that was running perfectly well. Filtering on the headSha field instead works
// across versions.
func TestWaitForImageBuildMatchesTheCommitClientSide(t *testing.T) {
	var gotArgs []string
	stubLocal(t, func(dir, name string, args []string) (string, error) {
		gotArgs = args
		// Newest first, and OTHER commits' runs are present — the filter has to
		// pick ours rather than simply taking the first row.
		return `[
		  {"databaseId":3,"status":"in_progress","conclusion":"","headSha":"newer-unrelated"},
		  {"databaseId":2,"status":"completed","conclusion":"success","headSha":"target"},
		  {"databaseId":1,"status":"completed","conclusion":"failure","headSha":"older-unrelated"}
		]`, nil
	})

	err := WaitForImageBuild(context.Background(), CIWaitOptions{
		SHA: "target", Workflow: "w.yaml", Poll: time.Millisecond, Timeout: time.Second,
	})
	if err != nil {
		t.Fatalf("should have matched the run for our commit: %v", err)
	}

	for _, arg := range gotArgs {
		if arg == "--commit" {
			t.Error("--commit is unsupported before gh 2.42 and errors out; match on headSha instead")
		}
	}
	if !strings.Contains(strings.Join(gotArgs, " "), "headSha") {
		t.Errorf("headSha must be requested so the commit can be matched: %v", gotArgs)
	}
}

// TestWaitForImageBuildIgnoresOtherCommits: a run for somebody else's push must
// not be mistaken for ours — reading its success would release an image this
// commit never produced.
func TestWaitForImageBuildIgnoresOtherCommits(t *testing.T) {
	calls := 0
	stubLocal(t, func(dir, name string, args []string) (string, error) {
		calls++
		if calls == 1 {
			return `[{"databaseId":9,"status":"completed","conclusion":"success","headSha":"someone-else"}]`, nil
		}
		return `[{"databaseId":9,"status":"completed","conclusion":"success","headSha":"someone-else"},
		         {"databaseId":10,"status":"completed","conclusion":"success","headSha":"mine"}]`, nil
	})

	if err := WaitForImageBuild(context.Background(), CIWaitOptions{
		SHA: "mine", Workflow: "w.yaml", Poll: time.Millisecond, Timeout: time.Second,
	}); err != nil {
		t.Fatalf("WaitForImageBuild: %v", err)
	}
	if calls < 2 {
		t.Error("a run for another commit must not end the wait")
	}
}

func TestDigestForTag(t *testing.T) {
	payload := `[
	  {"name":"sha256:aaa","metadata":{"container":{"tags":["latest","sha-deadbeef"]}}},
	  {"name":"sha256:bbb","metadata":{"container":{"tags":["sha-cafe"]}}}
	]`
	got, err := digestForTag(payload, "sha-cafe")
	if err != nil {
		t.Fatalf("digestForTag: %v", err)
	}
	if got != "sha256:bbb" {
		t.Errorf("digestForTag = %q, want sha256:bbb", got)
	}

	// An unpublished tag must be an explicit error, never a silent fallback to
	// some other version — that would pin the wrong image under --pin-digest.
	if _, err := digestForTag(payload, "sha-missing"); err == nil {
		t.Error("expected an error for a tag that is not in the registry")
	}
}

// TestResolveImageDigestRefusesNonGHCR: silently deploying a mutable tag when
// the user asked for a digest would make --pin-digest a lie.
func TestResolveImageDigestRefusesNonGHCR(t *testing.T) {
	stubLocal(t, func(dir, name string, args []string) (string, error) {
		t.Fatal("should not shell out for a non-ghcr registry")
		return "", nil
	})
	_, err := ResolveImageDigest(context.Background(), "", "docker.io/acme/app", "v1")
	if err == nil {
		t.Fatal("expected a refusal for a non-ghcr registry")
	}
	if !strings.Contains(err.Error(), "--image-tag") {
		t.Errorf("error should offer an alternative, got: %v", err)
	}
}

// TestResolveImageDigestParsesNestedPackages covers the encoding: ghcr package
// names keep every path segment after the owner and are passed to the API as a
// single URL-encoded component.
func TestResolveImageDigestParsesNestedPackages(t *testing.T) {
	var requested string
	stubLocal(t, func(dir, name string, args []string) (string, error) {
		if name == "gh" && len(args) >= 2 && args[0] == "api" {
			requested = args[1]
			return `[{"name":"sha256:abc","metadata":{"container":{"tags":["sha-1"]}}}]`, nil
		}
		t.Fatalf("unexpected command %s %v", name, args)
		return "", nil
	})

	got, err := ResolveImageDigest(context.Background(), "", "ghcr.io/mklfarha/aburrides/aburrides", "sha-1")
	if err != nil {
		t.Fatalf("ResolveImageDigest: %v", err)
	}
	if got != "sha256:abc" {
		t.Errorf("digest = %q, want sha256:abc", got)
	}
	if !strings.Contains(requested, "aburrides%2Faburrides") {
		t.Errorf("nested package name must be URL-encoded, requested %q", requested)
	}
	if !strings.Contains(requested, "/mklfarha/") {
		t.Errorf("owner should be the first path segment, requested %q", requested)
	}
}

func TestImageWorkflowFileMatchesTheGenerator(t *testing.T) {
	if got, want := ImageWorkflowFile("aburrides"), "publish-aburrides-image.yaml"; got != want {
		t.Errorf("ImageWorkflowFile = %q, want %q", got, want)
	}
}
