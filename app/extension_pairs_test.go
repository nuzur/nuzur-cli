package app

import (
	"testing"
	"time"

	"github.com/nuzur/nuzur-cli/extensionrun"
)

func TestParseConnectionMode(t *testing.T) {
	cases := []struct {
		in      string
		want    connectionMode
		wantErr bool
	}{
		{in: "remote", want: connModeRemote},
		{in: "direct", want: connModeRemote},
		{in: "Direct", want: connModeRemote},
		{in: " local ", want: connModeLocal},
		{in: "agent", want: connModeLocal},
		{in: "sideways", wantErr: true},
		{in: "", wantErr: true},
	}
	for _, c := range cases {
		got, err := parseConnectionMode(c.in)
		if c.wantErr {
			if err == nil {
				t.Errorf("parseConnectionMode(%q): expected an error, got %q", c.in, got)
			}
			continue
		}
		if err != nil {
			t.Errorf("parseConnectionMode(%q): unexpected error %v", c.in, err)
			continue
		}
		if got != c.want {
			t.Errorf("parseConnectionMode(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestInferModeFromConfig(t *testing.T) {
	cases := []struct {
		name   string
		cfg    map[string]interface{}
		want   connectionMode
		wantOK bool
	}{
		{name: "local agent fields", cfg: map[string]interface{}{"local_agent": "a", "local_agent_schema": "public"}, want: connModeLocal, wantOK: true},
		{name: "remote connection field", cfg: map[string]interface{}{"connection": "c", "schema": "public"}, want: connModeRemote, wantOK: true},
		{name: "remote store only", cfg: map[string]interface{}{"store": "s"}, want: connModeRemote, wantOK: true},
		{name: "shared fields only", cfg: map[string]interface{}{"mode": "merge_existing_matching_entities"}},
		{name: "empty", cfg: nil},
	}
	for _, c := range cases {
		got, ok := inferModeFromConfig(c.cfg)
		if ok != c.wantOK {
			t.Errorf("%s: ok = %v, want %v", c.name, ok, c.wantOK)
			continue
		}
		if ok && got != c.want {
			t.Errorf("%s: mode = %q, want %q", c.name, got, c.want)
		}
	}
}

func TestDecidePairMember(t *testing.T) {
	pair := mustPairForFront("sql-push")
	older := time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC)
	newer := time.Date(2026, 7, 20, 0, 0, 0, 0, time.UTC)

	cases := []struct {
		name        string
		requested   string
		modeFlag    string
		providedCfg map[string]interface{}
		entries     map[string]extensionrun.LastUsedEntry
		interactive bool
		wantID      string
		wantPrompt  bool
		wantSeed    connectionMode
		wantErr     bool
	}{
		{
			name:      "explicit local member implies local mode",
			requested: "sql-push-local",
			wantID:    "sql-push-local",
			wantSeed:  connModeLocal,
		},
		{
			name:      "explicit local member conflicts with remote flag",
			requested: "sql-push-local",
			modeFlag:  "remote",
			wantErr:   true,
		},
		{
			name:      "explicit local member agrees with local flag",
			requested: "sql-push-local",
			modeFlag:  "agent",
			wantID:    "sql-push-local",
			wantSeed:  connModeLocal,
		},
		{
			name:      "flag wins over config shape",
			requested: "sql-push",
			modeFlag:  "local",
			// a remote-shaped config must not override the explicit flag
			providedCfg: map[string]interface{}{"connection": "c"},
			wantID:      "sql-push-local",
			wantSeed:    connModeLocal,
		},
		{
			name:        "config shape infers local",
			requested:   "sql-push",
			providedCfg: map[string]interface{}{"local_agent": "a"},
			wantID:      "sql-push-local",
			wantSeed:    connModeLocal,
		},
		{
			name:        "config shape infers remote",
			requested:   "sql-push",
			providedCfg: map[string]interface{}{"store": "s", "connection": "c"},
			wantID:      "sql-push",
			wantSeed:    connModeRemote,
		},
		{
			name:      "non-interactive with no signal defaults to remote",
			requested: "sql-push",
			wantID:    "sql-push",
			wantSeed:  connModeRemote,
		},
		{
			name:        "interactive prompts seeded by most recent member",
			requested:   "sql-push",
			interactive: true,
			entries: map[string]extensionrun.LastUsedEntry{
				"sql-push":       {LastUsed: older},
				"sql-push-local": {LastUsed: newer},
			},
			wantPrompt: true,
			wantSeed:   connModeLocal,
		},
		{
			name:        "interactive prompt seeded remote when it ran last",
			requested:   "sql-push",
			interactive: true,
			entries: map[string]extensionrun.LastUsedEntry{
				"sql-push":       {LastUsed: newer},
				"sql-push-local": {LastUsed: older},
			},
			wantPrompt: true,
			wantSeed:   connModeRemote,
		},
		{
			name:        "interactive prompt defaults to remote with no history",
			requested:   "sql-push",
			interactive: true,
			wantPrompt:  true,
			wantSeed:    connModeRemote,
		},
	}

	for _, c := range cases {
		id, needPrompt, seed, err := decidePairMember(pair, c.requested, c.modeFlag, c.providedCfg, c.entries, c.interactive)
		if c.wantErr {
			if err == nil {
				t.Errorf("%s: expected an error, got id=%q", c.name, id)
			}
			continue
		}
		if err != nil {
			t.Errorf("%s: unexpected error %v", c.name, err)
			continue
		}
		if needPrompt != c.wantPrompt {
			t.Errorf("%s: needPrompt = %v, want %v", c.name, needPrompt, c.wantPrompt)
		}
		if !needPrompt && id != c.wantID {
			t.Errorf("%s: identifier = %q, want %q", c.name, id, c.wantID)
		}
		if seed != c.wantSeed {
			t.Errorf("%s: seed = %q, want %q", c.name, seed, c.wantSeed)
		}
	}
}

func TestAliasAwareLastConfig(t *testing.T) {
	pair := mustPairForFront("sql-import")
	older := time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC)
	newer := time.Date(2026, 7, 20, 0, 0, 0, 0, time.UTC)

	entries := map[string]extensionrun.LastUsedEntry{
		"sql-import": {
			LastUsed:     older,
			ConfigValues: map[string]interface{}{"store": "s", "connection": "c", "schema": "public", "mode": "replace_existing_matching_entities"},
		},
		"sql-import-local": {
			LastUsed:     newer,
			ConfigValues: map[string]interface{}{"local_agent": "a", "local_agent_connection": "ac", "mode": "merge_existing_matching_entities"},
		},
	}

	merged := aliasAwareLastConfig(pair, entries)

	// disjoint connection fields from both members survive the merge
	if merged["store"] != "s" || merged["local_agent"] != "a" {
		t.Errorf("expected both members' connection fields, got %v", merged)
	}
	// the shared field comes from the more recent run
	if merged["mode"] != "merge_existing_matching_entities" {
		t.Errorf("shared field should come from the newer entry, got %q", merged["mode"])
	}

	if got := aliasAwareLastConfig(pair, nil); got != nil {
		t.Errorf("expected nil for no entries, got %v", got)
	}
}

func TestFilterConfigToFields(t *testing.T) {
	merged := map[string]interface{}{
		"store": "s", "connection": "c", "schema": "public",
		"local_agent": "a", "local_agent_connection": "ac", "local_agent_schema": "app",
	}

	filtered := filterConfigToFields(merged, []string{"local_agent", "local_agent_connection", "local_agent_schema"})
	if len(filtered) != 3 {
		t.Fatalf("expected only the local member's fields, got %v", filtered)
	}
	for _, unwanted := range []string{"store", "connection", "schema"} {
		if _, ok := filtered[unwanted]; ok {
			t.Errorf("field %q from the other mode leaked through", unwanted)
		}
	}

	if got := filterConfigToFields(merged, []string{"nothing_matches"}); got != nil {
		t.Errorf("expected nil when nothing matches, got %v", got)
	}
	if got := filterConfigToFields(nil, []string{"store"}); got != nil {
		t.Errorf("expected nil for an empty config, got %v", got)
	}
}

// Deploy picks its sql-push member from the deployment's topology and passes the
// identifier straight to the backend, so these two strings are a wire contract:
// the registry must keep producing exactly what the old constants held.
func TestSqlPushPairMatchesDeployContract(t *testing.T) {
	if sqlPushPair.Front != "sql-push" {
		t.Errorf("deploy pushes to a team connection with %q, want \"sql-push\"", sqlPushPair.Front)
	}
	if sqlPushPair.Local != "sql-push-local" {
		t.Errorf("deploy pushes through the box agent with %q, want \"sql-push-local\"", sqlPushPair.Local)
	}
}

func TestPairRegistryLookups(t *testing.T) {
	for _, id := range []string{"sql-push", "sql-push-local", "sql-import", "sql-import-local"} {
		if _, ok := pairForIdentifier(id); !ok {
			t.Errorf("expected %q to belong to a pair", id)
		}
	}
	if _, ok := pairForIdentifier("go-code-gen"); ok {
		t.Error("go-code-gen must not be treated as a paired extension")
	}

	fronts := pairFrontIdentifiers()
	if len(fronts) != len(extensionPairs) {
		t.Fatalf("expected one front per pair, got %v", fronts)
	}
	for _, f := range fronts {
		pair, ok := pairForIdentifier(f)
		if !ok || pair.Front != f {
			t.Errorf("front %q does not resolve back to its pair", f)
		}
	}
}
