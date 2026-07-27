package extensionrun

import (
	"testing"

	nemgen "github.com/nuzur/nem/idl/gen"
)

func TestFilterRunnable(t *testing.T) {
	exts := []*nemgen.Extension{
		{Identifier: "go-code-gen", ExtensionType: nemgen.ExtensionType_EXTENSION_TYPE_GENERATOR},
		{Identifier: "sql-push", ExtensionType: nemgen.ExtensionType_EXTENSION_TYPE_SYNCHRONIZER},
		{Identifier: "sql-push-local", ExtensionType: nemgen.ExtensionType_EXTENSION_TYPE_SYNCHRONIZER},
		{Identifier: "sql-import", ExtensionType: nemgen.ExtensionType_EXTENSION_TYPE_IMPORTER},
		{Identifier: "sql-import-local", ExtensionType: nemgen.ExtensionType_EXTENSION_TYPE_IMPORTER},
		{Identifier: "some-other-importer", ExtensionType: nemgen.ExtensionType_EXTENSION_TYPE_IMPORTER},
	}
	fronts := map[string]bool{"sql-push": true, "sql-import": true}

	got := filterRunnable(exts, fronts)

	want := []string{"go-code-gen", "sql-push", "sql-import"}
	if len(got) != len(want) {
		t.Fatalf("got %d extensions, want %d: %v", len(got), len(want), identifiersOf(got))
	}
	for idx, id := range want {
		if got[idx].GetIdentifier() != id {
			t.Errorf("position %d = %q, want %q (server order must be preserved)", idx, got[idx].GetIdentifier(), id)
		}
	}
}

func identifiersOf(exts []*nemgen.Extension) []string {
	ids := make([]string, 0, len(exts))
	for _, e := range exts {
		ids = append(ids, e.GetIdentifier())
	}
	return ids
}
