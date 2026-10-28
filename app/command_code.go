package app

import (
	"bufio"
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"
	"time"

	nemgen "github.com/nuzur/nem/idl/gen"
	"github.com/nuzur/nuzur-cli/codescan"
	"github.com/nuzur/nuzur-cli/productclient"
	pb "github.com/nuzur/nuzur-cli/protodeps/gen"
	"github.com/urfave/cli"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/encoding/protojson"
)

// codeUploadTimeout covers inflating, validating and deriving findings for a
// large snapshot server-side, not just moving its (compressed) bytes.
const codeUploadTimeout = 2 * time.Minute

func (i *Implementation) CodeCommand() cli.Command {
	return cli.Command{
		Name:  "code",
		Usage: i.localize.Localize("code_desc", "Map a codebase's architecture into nuzur's Code view"),
		Subcommands: []cli.Command{
			i.CodeLinkCommand(),
			i.CodeScanCommand(),
			i.CodeStatusCommand(),
			i.CodeArchiveCommand(),
		},
	}
}

// ─── code link ───────────────────────────────────────────────────────────────

type codeLinkFlags struct {
	dir        string
	project    string
	identifier string
	yes        bool
}

func (i *Implementation) CodeLinkCommand() cli.Command {
	return cli.Command{
		Name:  "link",
		Usage: i.localize.Localize("code_link_desc", "Link the Go module in the current directory to a nuzur project"),
		Description: "Creates (or finds) the project's codebase for this repository and writes\n" +
			"   " + codescan.LinkFile + ", which `code scan` and agents read to know where snapshots go.\n" +
			"   Commit that file. Linking only adds a codebase to the project: it never touches\n" +
			"   the project's schema or data.",
		Flags: []cli.Flag{
			cli.StringFlag{Name: "dir", Value: ".", Usage: "Go module root to link"},
			cli.StringFlag{Name: "project", Usage: "Project name or uuid (default: detected from nuzur-generated code, else asked)"},
			cli.StringFlag{Name: "identifier", Usage: "Codebase identifier (default: the repository name)"},
			cli.BoolFlag{Name: "yes", Usage: "Do not ask for confirmation (required when not running in a terminal)"},
		},
		Action: func(c *cli.Context) error {
			if err := i.login(); err != nil {
				return err
			}
			return i.codeLink(context.Background(), codeLinkFlags{
				dir: c.String("dir"), project: c.String("project"), identifier: c.String("identifier"), yes: c.Bool("yes"),
			}, os.Stdin, os.Stdout)
		},
	}
}

func (i *Implementation) codeLink(ctx context.Context, flags codeLinkFlags, in io.Reader, out io.Writer) error {
	repo, err := codescan.DescribeRepo(ctx, flags.dir)
	if err != nil {
		return err
	}
	if existing, err := codescan.ReadLink(flags.dir); err == nil {
		fmt.Fprintf(out, "Already linked to codebase %s (%s). Re-linking refreshes it.\n", existing.Identifier, existing.CodebaseUUID)
	}

	project, err := i.codeLinkProject(flags)
	if err != nil {
		return err
	}
	identifier := flags.identifier
	if identifier == "" {
		identifier = repo.DefaultIdentifier()
	}

	fmt.Fprintf(out, "Link this module to nuzur:\n")
	fmt.Fprintf(out, "  project        %s (%s)\n", project.GetName(), project.GetUuid())
	fmt.Fprintf(out, "  identifier     %s\n", identifier)
	fmt.Fprintf(out, "  repository     %s\n", valueOr(repo.RemoteURL, "(no origin remote)"))
	fmt.Fprintf(out, "  module dir     %s\n", valueOr(repo.ModuleDir, "(repository root)"))
	fmt.Fprintf(out, "  default branch %s\n", valueOr(repo.DefaultBranch, "(unknown)"))
	if !flags.yes {
		if !isInteractive() {
			return errors.New("not running in a terminal: review the details above and pass --yes to link")
		}
		ok, err := askYesNo(in, out, "Link it?")
		if err != nil {
			return err
		}
		if !ok {
			return errors.New("not linked")
		}
	}

	authCtx, err := productclient.ClientContext()
	if err != nil {
		return err
	}
	res, err := i.productClient.ProductClient.CreateCodebase(authCtx, &pb.CreateCodebaseRequest{
		ProjectUuid:   project.GetUuid(),
		Identifier:    identifier,
		RepoUrl:       repo.RemoteURL,
		ModuleDir:     repo.ModuleDir,
		DefaultBranch: repo.DefaultBranch,
	})
	if err != nil {
		return err
	}
	cb := res.GetCodebase()
	if err := codescan.WriteLink(flags.dir, codescan.Link{
		ProjectUUID: project.GetUuid(), CodebaseUUID: cb.GetUuid(), Identifier: cb.GetIdentifier(),
	}); err != nil {
		return err
	}
	verb := "Linked to the existing"
	if res.GetCreated() {
		verb = "Created"
	}
	fmt.Fprintf(out, "%s codebase %s (%s) and wrote %s — commit it.\n", verb, cb.GetIdentifier(), cb.GetUuid(), codescan.LinkFile)
	fmt.Fprintf(out, "Next: nuzur-cli code scan\n")
	return nil
}

// codeLinkProject picks the project: --project, else the one stamped into the
// module's nuzur-generated code, else a picker.
func (i *Implementation) codeLinkProject(flags codeLinkFlags) (*nemgen.Project, error) {
	er, err := i.extensionRunner()
	if err != nil {
		return nil, err
	}
	if flags.project != "" {
		return i.resolveProject(er, flags.project)
	}
	stamped, err := codescan.StampedProjects(flags.dir)
	if err != nil {
		return nil, err
	}
	if len(stamped) == 1 {
		return i.resolveProject(er, stamped[0])
	}
	if !isInteractive() {
		if len(stamped) > 1 {
			return nil, fmt.Errorf("this module contains code generated for several projects (%s): pass --project", strings.Join(stamped, ", "))
		}
		return nil, errors.New("--project is required when not running in a terminal")
	}
	return i.SelectProject(er)
}

// ─── code scan ───────────────────────────────────────────────────────────────

func (i *Implementation) CodeScanCommand() cli.Command {
	return cli.Command{
		Name:  "scan",
		Usage: i.localize.Localize("code_scan_desc", "Extract the architecture facts of the app in the current directory: its Go code and the templates, scripts and stylesheets it serves"),
		Description: "Reads packages, imports, fx wiring, HTTP routes, nuzur entity calls, external systems,\n" +
			"   key-value keyspaces and background work, plus the HTML templates the Go code renders, the\n" +
			"   JavaScript and CSS they load and the routes that JavaScript calls. Run it from the Go module\n" +
			"   root. No source code is collected: only identifiers, repo-relative paths, route patterns,\n" +
			"   hosts, key prefixes, line counts and content hashes.\n" +
			"   When the module is linked (" + codescan.LinkFile + "), the snapshot is uploaded to nuzur;\n" +
			"   the first upload of a codebase is previewed and confirmed.",
		Flags: []cli.Flag{
			cli.StringFlag{Name: "dir", Value: ".", Usage: "Go module root to scan"},
			cli.StringFlag{Name: "out", Usage: "Write the facts as JSON to this file"},
			cli.IntFlag{Name: "churn-days", Value: 90, Usage: "Git history window for churn metrics (0 disables)"},
			cli.BoolFlag{Name: "allow-dirty", Usage: "Scan even when the working tree has uncommitted changes; the snapshot is marked dirty"},
			cli.BoolFlag{Name: "no-upload", Usage: "Scan only; do not upload even when the module is linked"},
			cli.BoolFlag{Name: "yes", Usage: "Do not ask before the first upload (for CI and agents)"},
			cli.BoolFlag{Name: "no-branch-point", Usage: "On a branch other than the default one, do not also scan the commit it split off at"},
		},
		Action: func(c *cli.Context) error {
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
			defer cancel()

			// The link decides where the snapshot goes and, through the
			// codebase, which branch is the default one.
			upload := !c.Bool("no-upload")
			var link codescan.Link
			if upload {
				var err error
				link, err = codescan.ReadLink(c.String("dir"))
				if errors.Is(err, os.ErrNotExist) {
					upload = false
				} else if err != nil {
					return err
				} else if err := i.login(); err != nil {
					return err
				}
			}
			opts := codescan.Options{Dir: c.String("dir"), ChurnDays: c.Int("churn-days")}
			if upload {
				opts.DefaultBranch = i.codebaseDefaultBranch(link)
			}

			started := time.Now()
			facts, err := codescan.Scan(ctx, opts)
			if err != nil {
				return err
			}
			if facts.Git.GetDirty() && !c.Bool("allow-dirty") {
				return fmt.Errorf("the working tree has uncommitted changes, so the facts would not match commit %s.\n"+
					"Commit first, or pass --allow-dirty to mark the snapshot dirty", shortSHA(facts.Git.GetCommitSha()))
			}
			if out := c.String("out"); out != "" {
				raw, err := protojson.MarshalOptions{Multiline: true, Indent: "  "}.Marshal(facts)
				if err != nil {
					return err
				}
				if err := os.WriteFile(out, raw, 0o644); err != nil {
					return err
				}
			}
			printCodeSummary(facts, time.Since(started))
			if !upload {
				if !c.Bool("no-upload") {
					fmt.Println("Not linked to nuzur, so nothing was uploaded. Run `nuzur-cli code link` to see this in the Code view.")
				}
				return nil
			}
			if err := i.codeUpload(link, facts, c.Bool("yes"), os.Stdin, os.Stdout); err != nil {
				return err
			}
			if c.Bool("no-branch-point") {
				return nil
			}
			return i.codeScanBranchPoint(link, facts, func(sha string) (*pb.CodeFacts, error) {
				bpOpts := opts
				bpOpts.Branch = facts.GetGit().GetDefaultBranch()
				return codescan.ScanCommit(ctx, bpOpts, sha)
			}, os.Stdout)
		},
	}
}

// codebaseDefaultBranch is the linked codebase's default branch as nuzur
// records it, so every checkout agrees on it; empty (the scanner then asks
// git) when it cannot be read.
func (i *Implementation) codebaseDefaultBranch(link codescan.Link) string {
	authCtx, err := productclient.ClientContext()
	if err != nil || link.ProjectUUID == "" {
		return ""
	}
	res, err := i.productClient.ProductClient.ListCodebasesForProject(authCtx, &pb.ListCodebasesForProjectRequest{ProjectUuid: link.ProjectUUID})
	if err != nil {
		return ""
	}
	for _, c := range res.GetCodebases() {
		if c.GetCodebase().GetUuid() == link.CodebaseUUID {
			return c.GetCodebase().GetDefaultBranch()
		}
	}
	return ""
}

// codeScanBranchPoint makes sure the commit a branch split off at is scanned
// too, so "what has this branch changed" has its base. It is scanned in a
// temporary worktree (scan), never in the user's checkout. A failure here is
// reported but does not fail the scan that was already uploaded.
func (i *Implementation) codeScanBranchPoint(link codescan.Link, facts *pb.CodeFacts, scan func(sha string) (*pb.CodeFacts, error), out io.Writer) error {
	git := facts.GetGit()
	sha := git.GetBranchPointSha()
	if sha == "" || sha == git.GetCommitSha() {
		return nil
	}
	authCtx, err := productclient.ClientContext()
	if err != nil {
		return err
	}
	_, err = i.productClient.ProductClient.GetCodeSnapshot(authCtx, &pb.GetCodeSnapshotRequest{CodebaseUuid: link.CodebaseUUID, CommitSha: sha, Lean: true})
	if err == nil {
		return nil
	}
	if status.Code(err) != codes.NotFound {
		return err
	}
	fmt.Fprintf(out, "Scanning %s, where %s split from %s, so its changes can be compared…\n", shortSHA(sha), git.GetBranch(), git.GetDefaultBranch())
	base, err := scan(sha)
	if err == nil {
		err = i.codeUpload(link, base, true, nil, out)
	}
	if err != nil {
		fmt.Fprintf(out, "Could not scan the branch point %s: %v\n", shortSHA(sha), err)
	}
	return nil
}

// codeUpload sends the facts to the linked codebase. The first upload of a
// codebase shows what leaves the machine and asks, unless --yes.
func (i *Implementation) codeUpload(link codescan.Link, facts *pb.CodeFacts, yes bool, in io.Reader, out io.Writer) error {
	authCtx, err := productclient.ClientContext()
	if err != nil {
		return err
	}
	existing, err := i.productClient.ProductClient.ListCodeSnapshots(authCtx, &pb.ListCodeSnapshotsRequest{CodebaseUuid: link.CodebaseUUID, PageSize: 1})
	if err != nil {
		return err
	}
	if len(existing.GetSnapshots()) == 0 && !yes {
		printUploadPreview(out, facts)
		if !isInteractive() {
			return errors.New("first upload of this codebase: review the preview above and pass --yes to upload")
		}
		ok, err := askYesNo(in, out, fmt.Sprintf("Upload this to codebase %s?", link.Identifier))
		if err != nil {
			return err
		}
		if !ok {
			return errors.New("not uploaded")
		}
	}

	body, err := gzipFacts(facts)
	if err != nil {
		return err
	}
	upCtx, cancel, err := productclient.ClientContextWithTimeout(codeUploadTimeout)
	if err != nil {
		return err
	}
	defer cancel()
	res, err := i.productClient.ProductClient.UploadCodeSnapshot(upCtx, &pb.UploadCodeSnapshotRequest{
		CodebaseUuid: link.CodebaseUUID, FactsGzip: body,
	})
	if err != nil {
		return err
	}
	verb := "Uploaded"
	if res.GetReplaced() {
		verb = "Replaced the earlier upload of"
	}
	fmt.Fprintf(out, "%s %s to codebase %s (%d KiB compressed).\n", verb, shortSHA(res.GetSnapshot().GetCommitSha()), link.Identifier, len(body)>>10)
	printFindingsBrief(out, res.GetSummary())
	return nil
}

func gzipFacts(facts *pb.CodeFacts) ([]byte, error) {
	raw, err := protojson.Marshal(facts)
	if err != nil {
		return nil, err
	}
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	if _, err := zw.Write(raw); err != nil {
		return nil, err
	}
	if err := zw.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// printUploadPreview shows, before the first upload, exactly the kinds of
// values that leave the machine, with samples.
func printUploadPreview(out io.Writer, f *pb.CodeFacts) {
	var externals, keyspaces, entities []string
	for _, n := range f.GetNodes() {
		switch n.GetKind() {
		case pb.CodeNodeKind_CODE_NODE_KIND_EXTERNAL_SYSTEM:
			externals = append(externals, n.GetName())
		case pb.CodeNodeKind_CODE_NODE_KIND_KEYSPACE:
			keyspaces = append(keyspaces, n.GetName())
		case pb.CodeNodeKind_CODE_NODE_KIND_ENTITY_REF:
			entities = append(entities, n.GetName())
		}
	}
	var routes []string
	for _, r := range f.GetRoutes() {
		routes = append(routes, r.GetMethod()+" "+r.GetPattern())
	}
	fmt.Fprintln(out, "This is the first upload for this codebase. It sends identifiers, repo-relative paths,")
	fmt.Fprintln(out, "route patterns, hosts, key prefixes and metrics — no source code. Samples:")
	fmt.Fprintf(out, "  module     %s @ %s (%s)\n", f.GetModulePath(), shortSHA(f.GetGit().GetCommitSha()), valueOr(f.GetGit().GetRemoteUrl(), "no remote"))
	fmt.Fprintf(out, "  routes     %s\n", sample(routes))
	fmt.Fprintf(out, "  hosts      %s\n", sample(externals))
	fmt.Fprintf(out, "  keyspaces  %s\n", sample(keyspaces))
	fmt.Fprintf(out, "  entities   %s\n", sample(entities))
	fmt.Fprintln(out, "Write the full payload with --out facts.json to inspect it.")
}

func sample(in []string) string {
	if len(in) == 0 {
		return "(none)"
	}
	sorted := append([]string(nil), in...)
	sort.Strings(sorted)
	const n = 5
	if len(sorted) <= n {
		return strings.Join(sorted, ", ")
	}
	return fmt.Sprintf("%s, … (%d in all)", strings.Join(sorted[:n], ", "), len(sorted))
}

func printFindingsBrief(out io.Writer, s *pb.CodeSummary) {
	if s == nil || len(s.GetFindings()) == 0 {
		fmt.Fprintln(out, "No findings.")
		return
	}
	fmt.Fprintf(out, "%d finding(s):\n", len(s.GetFindings()))
	for i, f := range s.GetFindings() {
		if i == 5 {
			fmt.Fprintf(out, "  … and %d more (ask your agent for getCodeFindings, or open the Code view)\n", len(s.GetFindings())-5)
			break
		}
		fmt.Fprintf(out, "  [%s] %s\n", f.GetSeverity(), f.GetTitle())
	}
}

// ─── code status ─────────────────────────────────────────────────────────────

func (i *Implementation) CodeStatusCommand() cli.Command {
	return cli.Command{
		Name:  "status",
		Usage: i.localize.Localize("code_status_desc", "Show the linked codebase and whether HEAD has been scanned"),
		Flags: []cli.Flag{
			cli.StringFlag{Name: "dir", Value: ".", Usage: "Go module root"},
		},
		Action: func(c *cli.Context) error {
			link, err := codescan.ReadLink(c.String("dir"))
			if errors.Is(err, os.ErrNotExist) {
				return errors.New("not linked: run `nuzur-cli code link`")
			}
			if err != nil {
				return err
			}
			if err := i.login(); err != nil {
				return err
			}
			ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
			defer cancel()
			repo, err := codescan.DescribeRepo(ctx, c.String("dir"))
			if err != nil {
				return err
			}
			return i.codeStatus(link, repo.Head, os.Stdout)
		},
	}
}

func (i *Implementation) codeStatus(link codescan.Link, head string, out io.Writer) error {
	authCtx, err := productclient.ClientContext()
	if err != nil {
		return err
	}
	fmt.Fprintf(out, "Codebase %s (%s), project %s\n", link.Identifier, link.CodebaseUUID, link.ProjectUUID)
	latest, err := i.productClient.ProductClient.GetCodeSnapshot(authCtx, &pb.GetCodeSnapshotRequest{CodebaseUuid: link.CodebaseUUID, Lean: true})
	if status.Code(err) == codes.NotFound {
		fmt.Fprintln(out, "Nothing scanned yet: run `nuzur-cli code scan`.")
		return nil
	}
	if err != nil {
		return err
	}
	s := latest.GetSnapshot()
	fmt.Fprintf(out, "Latest snapshot %s on %s, committed %s\n", shortSHA(s.GetCommitSha()), valueOr(s.GetBranch(), "?"),
		s.GetCommittedAt().AsTime().Local().Format("2006-01-02 15:04"))
	switch {
	case head == "":
	case head == s.GetCommitSha():
		fmt.Fprintln(out, "HEAD is scanned.")
	default:
		_, err := i.productClient.ProductClient.GetCodeSnapshot(authCtx, &pb.GetCodeSnapshotRequest{
			CodebaseUuid: link.CodebaseUUID, CommitSha: head, Lean: true,
		})
		if status.Code(err) == codes.NotFound {
			fmt.Fprintf(out, "HEAD (%s) has not been scanned: run `nuzur-cli code scan`.\n", shortSHA(head))
		} else if err == nil {
			fmt.Fprintf(out, "HEAD (%s) is scanned; the latest snapshot is a newer commit.\n", shortSHA(head))
		} else {
			return err
		}
	}
	printFindingsBrief(out, latest.GetSummary())
	return nil
}

// ─── code archive ────────────────────────────────────────────────────────────

func (i *Implementation) CodeArchiveCommand() cli.Command {
	return cli.Command{
		Name:  "archive",
		Usage: i.localize.Localize("code_archive_desc", "Hide the linked codebase from its nuzur project"),
		Description: "The codebase disappears from the project's Code view and from MCP, and uploads stop.\n" +
			"   Nothing is deleted: `nuzur-cli code link` in the repository brings it back.",
		Flags: []cli.Flag{
			cli.StringFlag{Name: "dir", Value: ".", Usage: "Go module root"},
			cli.BoolFlag{Name: "yes", Usage: "Do not ask for confirmation (required when not running in a terminal)"},
		},
		Action: func(c *cli.Context) error {
			link, err := codescan.ReadLink(c.String("dir"))
			if errors.Is(err, os.ErrNotExist) {
				return errors.New("not linked: there is no codebase to archive")
			}
			if err != nil {
				return err
			}
			if err := i.login(); err != nil {
				return err
			}
			return i.codeArchive(link, c.Bool("yes"), os.Stdin, os.Stdout)
		},
	}
}

func (i *Implementation) codeArchive(link codescan.Link, yes bool, in io.Reader, out io.Writer) error {
	if !yes {
		if !isInteractive() {
			return fmt.Errorf("not running in a terminal: pass --yes to archive codebase %s", link.Identifier)
		}
		ok, err := askYesNo(in, out, fmt.Sprintf("Archive codebase %s (%s)?", link.Identifier, link.CodebaseUUID))
		if err != nil {
			return err
		}
		if !ok {
			return errors.New("not archived")
		}
	}
	authCtx, err := productclient.ClientContext()
	if err != nil {
		return err
	}
	if _, err := i.productClient.ProductClient.ArchiveCodebase(authCtx, &pb.ArchiveCodebaseRequest{CodebaseUuid: link.CodebaseUUID}); err != nil {
		return err
	}
	fmt.Fprintf(out, "Archived codebase %s. %s still points at it; `nuzur-cli code link` restores it.\n", link.Identifier, codescan.LinkFile)
	return nil
}

// ─── helpers ─────────────────────────────────────────────────────────────────

// askYesNo asks a y/N question; anything but y/yes is no, and end of input
// without an answer is an error rather than a silent no.
func askYesNo(in io.Reader, out io.Writer, question string) (bool, error) {
	fmt.Fprintf(out, "%s [y/N] ", question)
	line, err := bufio.NewReader(in).ReadString('\n')
	if err != nil && strings.TrimSpace(line) == "" {
		return false, fmt.Errorf("no answer: %w", err)
	}
	switch strings.ToLower(strings.TrimSpace(line)) {
	case "y", "yes":
		return true, nil
	}
	return false, nil
}

func valueOr(v, fallback string) string {
	if v == "" {
		return fallback
	}
	return v
}

func printCodeSummary(f *pb.CodeFacts, took time.Duration) {
	kinds := map[pb.CodeNodeKind]int{}
	for _, n := range f.Nodes {
		kinds[n.Kind]++
	}
	edges := map[pb.CodeEdgeKind]int{}
	for _, e := range f.Edges {
		edges[e.Kind]++
	}
	fmt.Printf("Scanned %s at %s in %s\n", f.ModulePath, shortSHA(f.Git.GetCommitSha()), took.Round(time.Millisecond))
	fmt.Printf("  containers %d · components %d · packages %d · generated %d\n",
		kinds[pb.CodeNodeKind_CODE_NODE_KIND_CONTAINER], kinds[pb.CodeNodeKind_CODE_NODE_KIND_COMPONENT],
		kinds[pb.CodeNodeKind_CODE_NODE_KIND_PACKAGE], kinds[pb.CodeNodeKind_CODE_NODE_KIND_GENERATED])
	fmt.Printf("  entities %d · externals %d · keyspaces %d\n",
		kinds[pb.CodeNodeKind_CODE_NODE_KIND_ENTITY_REF], kinds[pb.CodeNodeKind_CODE_NODE_KIND_EXTERNAL_SYSTEM],
		kinds[pb.CodeNodeKind_CODE_NODE_KIND_KEYSPACE])
	fmt.Printf("  routes %d · background tasks %d · import edges %d · injects %d\n",
		len(f.Routes), len(f.Background), edges[pb.CodeEdgeKind_CODE_EDGE_KIND_IMPORTS], edges[pb.CodeEdgeKind_CODE_EDGE_KIND_INJECTS])
	if len(f.LoadErrors) > 0 {
		ids := make([]string, 0, len(f.LoadErrors))
		for _, e := range f.LoadErrors {
			ids = append(ids, e.PackageId)
		}
		sort.Strings(ids)
		fmt.Printf("  %d package(s) did not type-check and may be incomplete: %v\n", len(ids), ids)
	}
	if len(f.Unresolved) > 0 {
		fmt.Printf("  %d call(s) could not be mapped (see \"unresolved\" in the facts)\n", len(f.Unresolved))
	}
}
