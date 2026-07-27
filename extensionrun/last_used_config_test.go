package extensionrun

import (
	"encoding/json"
	"testing"
	"time"
)

func TestUpsertExtensionMetadataPreservesSiblings(t *testing.T) {
	sibling := extensionMetadata{
		LastUsed:            "2026-07-01T10:00:00Z",
		ConfigValues:        `{"store":"s","connection":"c","schema":"public"}`,
		ExtensionVersion:    "0.0.2",
		ExtensionIdentifier: "sql-push",
	}
	existing, err := json.Marshal(map[string]interface{}{
		"DataManagerMetadata": map[string]interface{}{"tabs": []string{}},
		"ExtensionsMetadata":  map[string]extensionMetadata{"sql-push": sibling},
	})
	if err != nil {
		t.Fatal(err)
	}

	now := time.Date(2026, 7, 20, 12, 0, 0, 0, time.UTC)
	out, err := upsertExtensionMetadata(existing, "sql-push-local", map[string]interface{}{"local_agent": "a"}, now)
	if err != nil {
		t.Fatal(err)
	}

	var raw map[string]json.RawMessage
	if err := json.Unmarshal(out, &raw); err != nil {
		t.Fatal(err)
	}
	if _, ok := raw["DataManagerMetadata"]; !ok {
		t.Error("unrelated top-level keys must be preserved")
	}

	var meta map[string]extensionMetadata
	if err := json.Unmarshal(raw["ExtensionsMetadata"], &meta); err != nil {
		t.Fatal(err)
	}

	// The sibling is what decides the default mode next time, so nothing about
	// it may change when the other member runs.
	if got := meta["sql-push"]; got != sibling {
		t.Errorf("sibling entry changed:\n got %+v\nwant %+v", got, sibling)
	}

	local := meta["sql-push-local"]
	if local.LastUsed != now.Format(time.RFC3339Nano) {
		t.Errorf("target lastUsed = %q, want %q", local.LastUsed, now.Format(time.RFC3339Nano))
	}
	if local.ExtensionIdentifier != "sql-push-local" {
		t.Errorf("target identifier = %q", local.ExtensionIdentifier)
	}
	var cfg map[string]interface{}
	if err := json.Unmarshal([]byte(local.ConfigValues), &cfg); err != nil {
		t.Fatal(err)
	}
	if cfg["local_agent"] != "a" {
		t.Errorf("target config values = %v", cfg)
	}
}

func TestUpsertExtensionMetadataKeepsExtensionVersionOnUpdate(t *testing.T) {
	existing, err := json.Marshal(map[string]interface{}{
		"ExtensionsMetadata": map[string]extensionMetadata{
			"sql-push": {LastUsed: "2026-07-01T10:00:00Z", ConfigValues: `{"store":"old"}`, ExtensionVersion: "0.0.2", ExtensionIdentifier: "sql-push"},
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	now := time.Date(2026, 7, 20, 12, 0, 0, 0, time.UTC)
	out, err := upsertExtensionMetadata(existing, "sql-push", map[string]interface{}{"store": "new"}, now)
	if err != nil {
		t.Fatal(err)
	}

	var pvd projectVersionData
	if err := json.Unmarshal(out, &pvd); err != nil {
		t.Fatal(err)
	}
	entry := pvd.ExtensionsMetadata["sql-push"]
	if entry.ExtensionVersion != "0.0.2" {
		t.Errorf("extensionVersion should survive an update, got %q", entry.ExtensionVersion)
	}
	if entry.ConfigValues != `{"store":"new"}` {
		t.Errorf("configValues = %s", entry.ConfigValues)
	}
}

func TestUpsertExtensionMetadataFromEmpty(t *testing.T) {
	now := time.Date(2026, 7, 20, 12, 0, 0, 0, time.UTC)
	out, err := upsertExtensionMetadata(nil, "sql-import", map[string]interface{}{"store": "s"}, now)
	if err != nil {
		t.Fatal(err)
	}
	var pvd projectVersionData
	if err := json.Unmarshal(out, &pvd); err != nil {
		t.Fatal(err)
	}
	if _, ok := pvd.ExtensionsMetadata["sql-import"]; !ok {
		t.Errorf("expected the entry to be created, got %v", pvd.ExtensionsMetadata)
	}
}

func TestUpsertExtensionMetadataMalformedExisting(t *testing.T) {
	now := time.Date(2026, 7, 20, 12, 0, 0, 0, time.UTC)
	out, err := upsertExtensionMetadata([]byte("not json"), "sql-push", map[string]interface{}{"store": "s"}, now)
	if err != nil {
		t.Fatal(err)
	}
	var pvd projectVersionData
	if err := json.Unmarshal(out, &pvd); err != nil {
		t.Fatal(err)
	}
	if _, ok := pvd.ExtensionsMetadata["sql-push"]; !ok {
		t.Error("a corrupt record should be replaced rather than fail the save")
	}
}
