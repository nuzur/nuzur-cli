package deploy

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

// ImageTagForSHA is the tag the generated workflow publishes for a commit.
//
// It must match `type=sha,format=long` in docker/metadata-action, which emits
// sha-<full 40-char sha>. The short form (type=sha with no format) would be
// sha-<12 chars> — close enough to look right in a log and wrong enough that
// every pull 404s.
func ImageTagForSHA(sha string) string { return "sha-" + strings.TrimSpace(sha) }

// ImageWorkflowFile is the filename the generator emits for the image build.
func ImageWorkflowFile(identifier string) string {
	return fmt.Sprintf("publish-%s-image.yaml", identifier)
}

// ciRun is the slice of `gh run list --json` this needs.
type ciRun struct {
	DatabaseID int64  `json:"databaseId"`
	Status     string `json:"status"`     // queued | in_progress | completed
	Conclusion string `json:"conclusion"` // success | failure | cancelled | ...
	HeadSHA    string `json:"headSha"`
}

// ciRunListLimit is how many recent runs of the workflow to fetch before
// filtering for our commit.
//
// The commit is matched HERE rather than with `gh run list --commit`, which
// only exists in gh 2.42+ — on anything older that flag is not "ignored", it is
// a hard `unknown flag` error, so the wait died on a perfectly good build. The
// headSha field has been available far longer, so filtering client-side works
// across versions. The limit is generous enough to survive a few unrelated
// pushes landing while a build is queued.
const ciRunListLimit = "30"

// CIWaitOptions bounds the wait for a CI build.
type CIWaitOptions struct {
	RepoRoot   string
	SHA        string
	Workflow   string        // workflow filename
	Timeout    time.Duration // total budget
	Poll       time.Duration // interval between checks
	OnProgress func(string)  // optional status line, called on each state change
}

// WaitForImageBuild blocks until the image workflow for a commit succeeds.
//
// It polls `gh run list` rather than shelling to `gh run watch` for two
// reasons: the run does not exist the instant a push lands (so watch would exit
// non-zero on a race that is not a failure), and polling lets the caller report
// the intermediate states, which matters when the wait is minutes long.
func WaitForImageBuild(ctx context.Context, opts CIWaitOptions) error {
	if err := RequireLocalTool("gh", "wait for the CI build",
		"install the GitHub CLI (https://cli.github.com) and run `gh auth login`, or pass --no-wait"); err != nil {
		return err
	}
	if opts.Poll <= 0 {
		opts.Poll = 10 * time.Second
	}
	if opts.Timeout <= 0 {
		opts.Timeout = 20 * time.Minute
	}

	deadline := time.Now().Add(opts.Timeout)
	var lastReported string
	report := func(state string) {
		if state != lastReported && opts.OnProgress != nil {
			opts.OnProgress(state)
			lastReported = state
		}
	}

	for {
		run, err := latestRunForSHA(ctx, opts)
		switch {
		case err != nil:
			return err
		case run == nil:
			report("waiting for the CI run to appear")
		case run.Status != "completed":
			report("CI build " + run.Status)
		case run.Conclusion == "success":
			report("CI build succeeded")
			return nil
		default:
			return fmt.Errorf(
				"the CI image build for %s finished with %q — the image was not published.\nSee: gh run view %d --log-failed",
				shortSHA(opts.SHA), run.Conclusion, run.DatabaseID)
		}

		if time.Now().After(deadline) {
			return fmt.Errorf(
				"timed out after %s waiting for the CI image build of %s.\nCheck `gh run list --commit %s`, then re-run with --no-wait once the image is published",
				opts.Timeout, shortSHA(opts.SHA), opts.SHA)
		}

		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(opts.Poll):
		}
	}
}

// latestRunForSHA returns the most recent run of the workflow for this commit,
// or nil when GitHub has not registered one yet.
func latestRunForSHA(ctx context.Context, opts CIWaitOptions) (*ciRun, error) {
	out, err := LocalCommand(ctx, opts.RepoRoot, "gh", "run", "list",
		"--workflow", opts.Workflow,
		"--limit", ciRunListLimit,
		"--json", "databaseId,status,conclusion,headSha")
	if err != nil {
		return nil, fmt.Errorf("querying CI runs for %s: %w", shortSHA(opts.SHA), err)
	}
	var runs []ciRun
	if err := json.Unmarshal([]byte(out), &runs); err != nil {
		return nil, fmt.Errorf("parsing `gh run list` output: %w", err)
	}
	// gh returns newest first, so the first match is the latest run for this
	// commit — a re-run supersedes the one it replaced.
	for i := range runs {
		if runs[i].HeadSHA == opts.SHA {
			return &runs[i], nil
		}
	}
	return nil, nil
}

// ResolveImageDigest returns the immutable sha256 digest for repository:tag.
//
// Implemented against the GitHub Packages API because ghcr.io is where this
// flow publishes and `gh` is already required to wait for the build — pulling
// in docker or crane just to read a manifest would add a dependency for one
// lookup. Other registries are refused explicitly rather than silently falling
// back to a mutable tag, which would make --pin-digest a lie.
func ResolveImageDigest(ctx context.Context, repoRoot, repository, tag string) (string, error) {
	const ghcrPrefix = "ghcr.io/"
	if !strings.HasPrefix(repository, ghcrPrefix) {
		return "", fmt.Errorf(
			"--pin-digest can only resolve digests on ghcr.io, and this image is %q.\nDeploy the tag instead (drop --pin-digest), or pass the digest yourself with --image-tag",
			repository)
	}
	if err := RequireLocalTool("gh", "resolve the image digest",
		"install the GitHub CLI (https://cli.github.com), or drop --pin-digest"); err != nil {
		return "", err
	}

	// ghcr.io/<owner>/<package path...>. The package name keeps the remaining
	// path segments and is URL-encoded, since the API takes it as one path
	// component: ghcr.io/acme/repo/svc -> owner acme, package "repo%2Fsvc".
	rest := strings.TrimPrefix(repository, ghcrPrefix)
	owner, pkg, ok := strings.Cut(rest, "/")
	if !ok || pkg == "" {
		return "", fmt.Errorf("cannot parse owner and package out of image %q", repository)
	}
	encoded := strings.ReplaceAll(pkg, "/", "%2F")

	// Users and orgs are different endpoints and nothing in the image name says
	// which this owner is, so try both before giving up.
	var lastErr error
	for _, scope := range []string{"users", "orgs"} {
		path := fmt.Sprintf("/%s/%s/packages/container/%s/versions?per_page=100", scope, owner, encoded)
		out, err := LocalCommand(ctx, repoRoot, "gh", "api", path)
		if err != nil {
			lastErr = err
			continue
		}
		digest, err := digestForTag(out, tag)
		if err != nil {
			return "", err
		}
		return digest, nil
	}
	return "", fmt.Errorf("looking up %s on ghcr: %w", repository, lastErr)
}

// digestForTag finds the version carrying tag in a GitHub Packages response.
func digestForTag(payload, tag string) (string, error) {
	var versions []struct {
		Name     string `json:"name"` // the sha256 digest
		Metadata struct {
			Container struct {
				Tags []string `json:"tags"`
			} `json:"container"`
		} `json:"metadata"`
	}
	if err := json.Unmarshal([]byte(payload), &versions); err != nil {
		return "", fmt.Errorf("parsing the ghcr package listing: %w", err)
	}
	for _, v := range versions {
		for _, t := range v.Metadata.Container.Tags {
			if t == tag {
				if !strings.HasPrefix(v.Name, "sha256:") {
					return "", fmt.Errorf("ghcr returned %q for tag %q, which is not a digest", v.Name, tag)
				}
				return v.Name, nil
			}
		}
	}
	return "", fmt.Errorf("no image tagged %q found in the registry yet — if CI is still publishing, retry in a moment", tag)
}

func shortSHA(sha string) string {
	if len(sha) > 7 {
		return sha[:7]
	}
	return sha
}
