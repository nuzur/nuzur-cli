package codescan

import (
	"slices"
	"strings"
	"testing"

	pb "github.com/nuzur/nuzur-cli/protodeps/gen"
)

func TestFrontend(t *testing.T) {
	f := scanFixture(t, Options{})
	const (
		season = "example.com/app/modules/season"
		show   = "example.com/app/modules/show"
	)
	seasonComp := nodeByID(f, season).GetParentId()
	showComp := nodeByID(f, show).GetParentId()
	R, I, C := pb.CodeEdgeKind_CODE_EDGE_KIND_RENDERS, pb.CodeEdgeKind_CODE_EDGE_KIND_INCLUDES, pb.CodeEdgeKind_CODE_EDGE_KIND_CALLS_ROUTE

	nodes := map[string]struct {
		kind   pb.CodeNodeKind
		path   string
		parent string
	}{
		"tpl:templates/season/season.html": {pb.CodeNodeKind_CODE_NODE_KIND_TEMPLATE, "templates/season/season.html", seasonComp},
		"tpl:templates/show.html":          {pb.CodeNodeKind_CODE_NODE_KIND_TEMPLATE, "templates/show.html", showComp},
		// Both pages include the header, so it belongs to neither.
		"tpl:templates/header.html": {pb.CodeNodeKind_CODE_NODE_KIND_TEMPLATE, "templates/header.html", ""},
		"tpl:templates/unused.html": {pb.CodeNodeKind_CODE_NODE_KIND_TEMPLATE, "templates/unused.html", ""},
		// Version suffixes stay in path but not in the id.
		"js:assets/js/season.js":        {pb.CodeNodeKind_CODE_NODE_KIND_SCRIPT, "assets/js/season_v1.2.0.js", seasonComp},
		"js:assets/js/lazy.js":          {pb.CodeNodeKind_CODE_NODE_KIND_SCRIPT, "assets/js/lazy_v1.2.0.js", seasonComp},
		"js:assets/js/util.js":          {pb.CodeNodeKind_CODE_NODE_KIND_SCRIPT, "assets/js/util.js", seasonComp},
		"js:assets/js/season-extra.js":  {pb.CodeNodeKind_CODE_NODE_KIND_SCRIPT, "assets/js/season-extra_v1.2.0.js", seasonComp},
		"js:assets/js/footer_widget.js": {pb.CodeNodeKind_CODE_NODE_KIND_SCRIPT, "assets/js/footer_widget.js", showComp},
		"js:assets/js/orphan.js":        {pb.CodeNodeKind_CODE_NODE_KIND_SCRIPT, "assets/js/orphan.js", ""},
	}
	for id, want := range nodes {
		n := nodeByID(f, id)
		if n == nil {
			t.Errorf("missing node %s", id)
			continue
		}
		if n.Kind != want.kind || n.Path != want.path || n.ParentId != want.parent {
			t.Errorf("%s = kind %v path %q parent %q, want %v %q %q", id, n.Kind, n.Path, n.ParentId, want.kind, want.path, want.parent)
		}
		if n.GetMetrics().GetLoc() == 0 {
			t.Errorf("%s has no lines counted", id)
		}
	}
	for _, n := range f.Nodes {
		if strings.Contains(n.Id, "vendor.min") || strings.Contains(n.Id, "cdn.example.com") {
			t.Errorf("vendored or remote script became a node: %s", n.Id)
		}
	}

	for _, e := range []struct {
		kind     pb.CodeEdgeKind
		from, to string
	}{
		{R, season, "tpl:templates/season/season.html"},
		{R, season, "tpl:templates/header.html"},
		{R, show, "tpl:templates/show.html"},                                 // "./templates/" + name
		{I, "tpl:templates/season/season.html", "tpl:templates/header.html"}, // {{define "header"}}
		{I, "tpl:templates/header.html", "tpl:templates/footer.html"},        // by file name
		{I, "tpl:templates/season/season.html", "js:assets/js/season.js"},    // {{ }} only in the query
		{I, "tpl:templates/season/season.html", "js:assets/js/lazy.js"},      // import() in an inline script
		{I, "js:assets/js/season.js", "js:assets/js/util.js"},                // relative import with a query
		{I, "js:assets/js/season.js", "js:assets/js/season-extra.js"},        // loader by name
		{I, show, "js:assets/js/footer_widget.js"},                           // Go names it for {{$f}}.js
	} {
		if edgeOf(f, e.kind, e.from, e.to) == nil {
			t.Errorf("missing %v %s -> %s", e.kind, e.from, e.to)
		}
	}

	calls := edgeOf(f, C, "js:assets/js/season.js", season)
	if calls == nil || !slices.Equal(calls.Ops, []string{"GET /season/{id}", "POST /admin/reset"}) {
		t.Errorf("season.js should call GET /season/{id} and POST /admin/reset, got %+v", calls)
	}
	if edgeOf(f, C, "js:assets/js/lazy.js", show) == nil {
		t.Error("lazy.js calls /show from a template literal")
	}
	if edgeOf(f, C, "js:assets/js/season.js", show) != nil {
		t.Error("a commented-out fetch must not count")
	}
}

func TestRouteMatches(t *testing.T) {
	for _, tc := range []struct {
		pattern, path string
		want          bool
	}{
		{"/season/{id}", "/season/" + dynamicSeg, true},
		{"/season/{id}", "/season/12", true},
		{"/season/{id:[0-9]+}", "/season/12", true},
		{"/season/{id}", "/season/12/edit", false},
		{"/fantasy/about", "/fantasy/" + dynamicSeg, false}, // a dynamic segment only fills a parameter
		{"/files/*", "/files/a/b", true},
		{"GET /items/{id...}", "/items/a/b", true}, // net/http pattern
		{"/", "/", false},
		{"/assets/{name}", "/assets/logo.png", false},
		{"/search", "/search?q=" + dynamicSeg, true},
	} {
		if got := routeMatches(tc.pattern, tc.path); got != tc.want {
			t.Errorf("routeMatches(%q, %q) = %v, want %v", tc.pattern, strings.ReplaceAll(tc.path, dynamicSeg, "<dyn>"), got, tc.want)
		}
	}
}

func TestNormalizeAssetPath(t *testing.T) {
	for in, want := range map[string]string{
		"assets/js/fsv3_v3.3.2.js":       "assets/js/fsv3.js",
		"assets/js/baq_v3.3.2/main.js":   "assets/js/baq/main.js",
		"assets/js/recap_loading-2.1.js": "assets/js/recap_loading.js",
		"templates/fantasyv3/start.html": "templates/fantasyv3/start.html", // v3 without dots is a name
		"assets/js/anime.js":             "assets/js/anime.js",
	} {
		if got := normalizeAssetPath(in); got != want {
			t.Errorf("normalizeAssetPath(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestJSPathStrings(t *testing.T) {
	src := "fetch('/a/' + id + '/b');\n" +
		"fetch(`/c/${x}/d`);\n" +
		"/* fetch('/hidden') */ const s = \"/e\";\n" +
		"const q = 'it\\'s' + '/not-a-path';\n"
	var got []string
	for _, p := range jsPathStrings(src) {
		got = append(got, strings.ReplaceAll(p.path, dynamicSeg, "*"))
	}
	want := []string{"/a/*/b", "/c/*/d", "/e", "/not-a-path"}
	if !slices.Equal(got, want) {
		t.Fatalf("jsPathStrings = %q, want %q", got, want)
	}
}
