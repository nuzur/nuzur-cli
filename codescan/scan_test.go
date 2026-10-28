package codescan

import (
	"bytes"
	"context"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"testing"

	pb "github.com/nuzur/nuzur-cli/protodeps/gen"
	"google.golang.org/protobuf/encoding/protojson"
)

// copyFixture copies testdata/app into a temp dir outside any git repository,
// so paths are relative to the fixture and no git facts leak in.
func copyFixture(t *testing.T) string {
	t.Helper()
	dst := t.TempDir()
	src := filepath.Join("testdata", "app")
	err := filepath.WalkDir(src, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(src, path)
		target := filepath.Join(dst, rel)
		if d.IsDir() {
			return os.MkdirAll(target, 0o755)
		}
		raw, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		return os.WriteFile(target, raw, 0o644)
	})
	if err != nil {
		t.Fatal(err)
	}
	return dst
}

func scanFixture(t *testing.T, opts Options) *pb.CodeFacts {
	t.Helper()
	opts.Dir = copyFixture(t)
	facts, err := Scan(context.Background(), opts)
	if err != nil {
		t.Fatalf("Scan: %v", err)
	}
	return facts
}

func nodeByID(f *pb.CodeFacts, id string) *pb.CodeNode {
	for _, n := range f.Nodes {
		if n.Id == id {
			return n
		}
	}
	return nil
}

func edgeOf(f *pb.CodeFacts, kind pb.CodeEdgeKind, from, to string) *pb.CodeEdge {
	for _, e := range f.Edges {
		if e.Kind == kind && e.From == from && e.To == to {
			return e
		}
	}
	return nil
}

const mod = "example.com/app"

func TestScanContainersAndComponents(t *testing.T) {
	f := scanFixture(t, Options{})
	if f.ModulePath != mod || f.SchemaVersion != SchemaVersion || f.Language != "go" {
		t.Fatalf("header = %q v%d %q", f.ModulePath, f.SchemaVersion, f.Language)
	}
	for _, id := range []string{mod, mod + "/tasks/reindex"} {
		if n := nodeByID(f, id); n == nil || n.Kind != pb.CodeNodeKind_CODE_NODE_KIND_CONTAINER {
			t.Errorf("%s should be a container, got %v", id, n)
		}
	}
	if n := nodeByID(f, mod); n.Name != "app" {
		t.Errorf("root container name = %q, want module basename", n.Name)
	}
	// modules/ holds two feature packages, so it is a namespace and each child
	// is its own component; fx-provided packages are flagged.
	season := nodeByID(f, mod+"/modules/season")
	if season == nil || season.ParentId != "component:modules/season" || season.Attrs["fx_provided"] != "true" {
		t.Fatalf("season package = %v", season)
	}
	if season.Doc != "Package season serves season pages." {
		t.Errorf("doc = %q, want first paragraph only", season.Doc)
	}
	comp := nodeByID(f, "component:modules/season")
	if comp == nil || comp.Kind != pb.CodeNodeKind_CODE_NODE_KIND_COMPONENT || comp.Metrics.GetLoc() == 0 {
		t.Fatalf("season component = %v", comp)
	}
	if got := comp.Docs; len(got) != 2 || got[0] != "docs/season_plan.md" || got[1] != "modules/season/README.md" {
		t.Errorf("component docs = %v", got)
	}
	if n := nodeByID(f, mod); n.Attrs["composition_root"] != "true" {
		t.Errorf("main calls fx.Provide, so it is a composition root: %v", n.Attrs)
	}
	if n := nodeByID(f, mod+"/store"); n == nil || n.ParentId != "component:store" {
		t.Errorf("store package = %v", n)
	}
}

func TestScanGeneratedTreesCollapse(t *testing.T) {
	f := scanFixture(t, Options{})
	for _, id := range []string{mod + "/sfapi/gen", mod + "/gen/core/module/drag"} {
		if nodeByID(f, id) != nil {
			t.Errorf("generated package %s must collapse, not be a node", id)
		}
	}
	gens := map[string]*pb.CodeGenerated{}
	for _, g := range f.Generated {
		gens[g.NodeId] = g
	}
	stamped := gens["gen:gen"]
	if stamped == nil || stamped.ProjectUuid != "11111111-1111-1111-1111-111111111111" || stamped.ProjectIdentifier != "drags" {
		t.Fatalf("stamped manifest = %v", stamped)
	}
	if n := nodeByID(f, "gen:gen"); n == nil || n.Name != "drags" {
		t.Errorf("generated node should take the project identifier as name, got %v", n)
	}
	if g := gens["gen:sfapi"]; g == nil || len(g.Packages) != 1 || g.Packages[0] != mod+"/sfapi/gen" {
		t.Errorf("sfapi manifest = %v", g)
	}
	if e := edgeOf(f, pb.CodeEdgeKind_CODE_EDGE_KIND_USES_GENERATED, mod+"/modules/season", "gen:sfapi"); e == nil {
		t.Error("missing season -> gen:sfapi edge")
	}
	// A generated tree with its own main is a deployable, and its imports of
	// hand-written code are kept: that is how a go-code-gen server wires its
	// custom zone in, which would otherwise look unreachable.
	if n := nodeByID(f, "gen:gen"); n.Attrs["main"] != "true" {
		t.Errorf("gen:gen has a generated main and should be marked main, attrs = %v", n.Attrs)
	}
	if n := nodeByID(f, "gen:sfapi"); n.Attrs["main"] == "true" {
		t.Error("gen:sfapi has no main and must not be marked")
	}
	if e := edgeOf(f, pb.CodeEdgeKind_CODE_EDGE_KIND_IMPORTS, "gen:gen", mod+"/modules/show"); e == nil {
		t.Error("missing gen:gen -> modules/show import edge")
	}
	if nodeByID(f, mod+"/gen/cmd/server") != nil {
		t.Error("the generated main must collapse into gen:gen, not become a container")
	}
}

func TestScanEntityCalls(t *testing.T) {
	f := scanFixture(t, Options{KnownEntities: []string{"season", "drag"}})
	season := mod + "/modules/season"
	if e := edgeOf(f, pb.CodeEdgeKind_CODE_EDGE_KIND_CALLS_ENTITY, season, "entity:season"); e == nil || len(e.Ops) != 2 || e.Ops[0] != "list" || e.Ops[1] != "update" {
		t.Errorf("season -> entity:season = %v, want ops list+update", e)
	}
	// go-code-gen core module call: entity from the package path.
	if e := edgeOf(f, pb.CodeEdgeKind_CODE_EDGE_KIND_CALLS_ENTITY, mod+"/modules/show", "entity:drag"); e == nil || len(e.Ops) != 2 || e.Ops[0] != "create" || e.Ops[1] != "get" {
		t.Errorf("show -> entity:drag = %v, want ops create+get", e)
	}
	if n := nodeByID(f, "entity:drag"); n == nil || n.ParentId != "gen:gen" {
		t.Errorf("entity:drag = %v, want parent gen:gen", n)
	}
	got := map[string]bool{}
	for _, u := range f.Unresolved {
		got[u.Kind+":"+u.Value] = true
	}
	if !got["entity_method:Ping"] || !got["unknown_entity:episode_has_drag"] || len(got) != 2 {
		t.Errorf("unresolved = %v", got)
	}
}

func TestScanFxRoutesExternalsKeyspacesBackground(t *testing.T) {
	f := scanFixture(t, Options{})
	if edgeOf(f, pb.CodeEdgeKind_CODE_EDGE_KIND_INJECTS, mod+"/modules/show", mod+"/modules/season") == nil ||
		edgeOf(f, pb.CodeEdgeKind_CODE_EDGE_KIND_INJECTS, mod+"/router", mod+"/modules/show") == nil {
		t.Error("missing fx injects edges")
	}

	var routes []string
	for _, r := range f.Routes {
		routes = append(routes, r.Method+" "+r.Pattern+" "+r.Handler)
	}
	want := []string{
		"POST /admin/reset season.Implementation.Reset",
		"GET /season/{id} season.Implementation.Handler",
		"* /show show.Implementation.Handler",
	}
	if len(routes) != len(want) {
		t.Fatalf("routes = %v", routes)
	}
	for i := range want {
		if routes[i] != want[i] {
			t.Errorf("route %d = %q, want %q", i, routes[i], want[i])
		}
	}
	if r := f.Routes[0]; len(r.Wrappers) != 1 || r.Wrappers[0] != "auth" || r.PackageId != mod+"/modules/season" {
		t.Errorf("wrapped route = %v", r)
	}

	if edgeOf(f, pb.CodeEdgeKind_CODE_EDGE_KIND_USES_EXTERNAL, mod+"/store", "ext:redis") == nil {
		t.Error("missing store -> redis sdk edge")
	}
	if edgeOf(f, pb.CodeEdgeKind_CODE_EDGE_KIND_USES_EXTERNAL, mod+"/modules/season", "ext:api.partner.net") == nil {
		t.Error("missing host literal edge")
	}
	if edgeOf(f, pb.CodeEdgeKind_CODE_EDGE_KIND_READS_KEYSPACE, mod+"/modules/season", "ks:redis:season:all") == nil {
		t.Error("missing static key read")
	}
	if edgeOf(f, pb.CodeEdgeKind_CODE_EDGE_KIND_WRITES_KEYSPACE, mod+"/modules/season", "ks:redis:season:") == nil {
		t.Error("missing Sprintf key prefix write")
	}

	kinds := map[pb.CodeBackgroundKind]int{}
	for _, b := range f.Background {
		kinds[b.Kind]++
	}
	if kinds[pb.CodeBackgroundKind_CODE_BACKGROUND_KIND_GOROUTINE] != 1 || kinds[pb.CodeBackgroundKind_CODE_BACKGROUND_KIND_TICKER] != 1 ||
		kinds[pb.CodeBackgroundKind_CODE_BACKGROUND_KIND_LIFECYCLE_HOOK] != 1 {
		t.Errorf("background = %v", f.Background)
	}
}

func TestScanIsDeterministic(t *testing.T) {
	dir := copyFixture(t)
	var out [][]byte
	for i := 0; i < 2; i++ {
		f, err := Scan(context.Background(), Options{Dir: dir})
		if err != nil {
			t.Fatal(err)
		}
		raw, err := protojson.MarshalOptions{Multiline: true}.Marshal(f)
		if err != nil {
			t.Fatal(err)
		}
		out = append(out, raw)
	}
	if !bytes.Equal(out[0], out[1]) {
		t.Error("two scans of the same tree differ")
	}
}

func TestSnakeCaseAndOps(t *testing.T) {
	for in, want := range map[string]string{
		"Season": "season", "EpisodeHasDrag": "episode_has_drag", "DragShowUUID": "drag_show_uuid",
		"FanstasySeasonHasDrag": "fanstasy_season_has_drag", "Season2Cast": "season2_cast",
	} {
		if got := snakeCase(in); got != want {
			t.Errorf("snakeCase(%q) = %q, want %q", in, got, want)
		}
	}
	for in, want := range map[string][2]string{
		"ListSeason": {"list", "Season"}, "FetchDragByUUID": {"get", "DragByUUID"},
		"Listen": {"", "Listen"}, "Upsert": {"update", ""},
	} {
		op, rest := splitOp(in)
		if op != want[0] || rest != want[1] {
			t.Errorf("splitOp(%q) = %q,%q want %v", in, op, rest, want)
		}
	}
	if i := byBoundary("DragByUUID"); i != 4 {
		t.Errorf("byBoundary = %d", i)
	}
	if i := byBoundary("Byline"); i != -1 {
		t.Errorf("byBoundary(Byline) = %d", i)
	}
}

func TestNormalizeRemote(t *testing.T) {
	for in, want := range map[string]string{
		"git@github.com:org/repo.git":             "https://github.com/org/repo",
		"https://tok:x@github.com/org/repo.git":   "https://github.com/org/repo",
		"https://github.com/org/repo":             "https://github.com/org/repo",
		"ssh://git@gitlab.com/group/sub/repo.git": "https://gitlab.com/group/sub/repo",
		"": "",
	} {
		if got := NormalizeRemote(in); got != want {
			t.Errorf("NormalizeRemote(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestMentionsAndJoinPattern(t *testing.T) {
	cases := []struct {
		path, name string
		want       bool
	}{
		{"docs/pass_gift_comp.md", "pass", true},
		{"docs/password_reset.md", "pass", false},
		{"docs/fantasy_v4/plan.md", "fantasyv4", true},
		{"docs/notes.md", "docs", false},
	}
	for _, c := range cases {
		if got := mentions(c.path, c.name); got != c.want {
			t.Errorf("mentions(%q, %q) = %v", c.path, c.name, got)
		}
	}
	got := []string{joinPattern("", "/a"), joinPattern("/admin", "/reset"), joinPattern("/api", "/")}
	want := []string{"/a", "/admin/reset", "/api/"}
	sort.Strings(got)
	sort.Strings(want)
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("joinPattern = %v, want %v", got, want)
		}
	}
}

func TestScanDeployHints(t *testing.T) {
	f := scanFixture(t, Options{})
	var got []string
	for _, h := range f.DeployHints {
		got = append(got, h.Kind+" "+h.File+" "+h.Value)
	}
	// replicas: 1 is not a hint, and the chart whose autoscaling is disabled
	// renders no HPA.
	want := []string{"autoscaler deploy/chart/templates/hpa.yaml 3", "autoscaler deploy/hpa.yaml "}
	if len(got) != len(want) || got[0] != want[0] || got[1] != want[1] {
		t.Errorf("deploy hints = %q, want %q", got, want)
	}
}
