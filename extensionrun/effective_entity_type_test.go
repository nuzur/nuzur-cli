package extensionrun

import (
	"testing"

	extensiongen "github.com/nuzur/extension-sdk/idl/gen"
)

func TestEffectiveEntityType(t *testing.T) {
	tests := []struct {
		name       string
		declared   extensiongen.EntityType
		identifier string
		want       extensiongen.EntityType
	}{
		// The sql-*-local extensions register these as bare uuid fields.
		{
			name:       "undeclared local_agent falls back",
			identifier: "local_agent",
			want:       extensiongen.EntityType_ENTITY_TYPE_LOCAL_AGENT,
		},
		{
			name:       "undeclared local_agent_connection falls back",
			identifier: "local_agent_connection",
			want:       extensiongen.EntityType_ENTITY_TYPE_LOCAL_AGENT_CONNECTION,
		},
		// A declared type always wins, so re-registering the extensions
		// correctly cannot be overridden by this shim.
		{
			name:       "declared type wins over the identifier",
			declared:   extensiongen.EntityType_ENTITY_TYPE_DB_CONNECTION,
			identifier: "local_agent",
			want:       extensiongen.EntityType_ENTITY_TYPE_DB_CONNECTION,
		},
		{
			name:       "declared local agent type passes through",
			declared:   extensiongen.EntityType_ENTITY_TYPE_LOCAL_AGENT,
			identifier: "local_agent",
			want:       extensiongen.EntityType_ENTITY_TYPE_LOCAL_AGENT,
		},
		{
			name:       "unrelated undeclared field stays invalid",
			identifier: "schema",
			want:       extensiongen.EntityType_ENTITY_TYPE_INVALID,
		},
		{
			name:       "declared store passes through",
			declared:   extensiongen.EntityType_ENTITY_TYPE_DB_STORE,
			identifier: "store",
			want:       extensiongen.EntityType_ENTITY_TYPE_DB_STORE,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := EffectiveEntityType(tt.declared, tt.identifier); got != tt.want {
				t.Fatalf("EffectiveEntityType(%v, %q) = %v, want %v", tt.declared, tt.identifier, got, tt.want)
			}
		})
	}
}
