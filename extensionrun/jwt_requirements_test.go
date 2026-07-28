package extensionrun

import (
	"strings"
	"testing"

	nemgen "github.com/nuzur/nem/idl/gen"
)

const (
	testIDFieldUUID       = "d1682705-1a89-4cc1-9b1f-e9a888c00001"
	testEmailFieldUUID    = "d1682705-1a89-4cc1-9b1f-e9a888c00002"
	testPasswordFieldUUID = "d1682705-1a89-4cc1-9b1f-e9a888c00003"
	testCreatedFieldUUID  = "d1682705-1a89-4cc1-9b1f-e9a888c00004"
)

// validUserEntity is a schema the generated JWT server can actually be built
// against, matching go-code-gen's reference fixture.
func validUserEntity() *nemgen.Entity {
	return &nemgen.Entity{
		Uuid:       "c1682705-1a89-4cc1-9b1f-e9a888c00000",
		Identifier: "user",
		Type:       nemgen.EntityType_ENTITY_TYPE_STANDALONE,
		Fields: []*nemgen.Field{
			{Uuid: testIDFieldUUID, Identifier: "id", Type: nemgen.FieldType_FIELD_TYPE_UUID, Key: true},
			{Uuid: testEmailFieldUUID, Identifier: "email", Type: nemgen.FieldType_FIELD_TYPE_VARCHAR},
			{Uuid: testPasswordFieldUUID, Identifier: "password", Type: nemgen.FieldType_FIELD_TYPE_VARCHAR},
			{Uuid: testCreatedFieldUUID, Identifier: "created_at", Type: nemgen.FieldType_FIELD_TYPE_DATETIME},
		},
		TypeConfig: &nemgen.EntityTypeConfig{
			Standalone: &nemgen.EntityTypeStandaloneConfig{
				Indexes: []*nemgen.Index{
					{
						Uuid:   "idx-pk",
						Type:   nemgen.IndexType_INDEX_TYPE_PRIMARY,
						Fields: []*nemgen.IndexField{{FieldUuid: testIDFieldUUID}},
					},
					{
						Uuid:   "idx-email",
						Type:   nemgen.IndexType_INDEX_TYPE_INDEX,
						Fields: []*nemgen.IndexField{{FieldUuid: testEmailFieldUUID}},
					},
				},
			},
		},
	}
}

func spanishUserEntity() *nemgen.Entity {
	entity := validUserEntity()
	entity.Identifier = "usuario"
	entity.Fields[1].Identifier = "correo"
	entity.Fields[2].Identifier = "contrasena"
	return entity
}

func TestCheckJWTAuthSchemaAcceptsEnglishAndSpanish(t *testing.T) {
	cases := []struct {
		name   string
		entity *nemgen.Entity
	}{
		{"english", validUserEntity()},
		{"spanish", spanishUserEntity()},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			missing, warnings := checkJWTAuthSchema([]*nemgen.Entity{tc.entity})
			if len(missing) != 0 {
				t.Fatalf("expected a valid schema to pass, got: %v", missing)
			}
			if len(warnings) != 0 {
				t.Fatalf("expected no warnings, got: %v", warnings)
			}
		})
	}
}

func TestCheckJWTAuthSchemaNoUserEntity(t *testing.T) {
	other := validUserEntity()
	other.Identifier = "account"

	missing, _ := checkJWTAuthSchema([]*nemgen.Entity{other})
	if len(missing) != 1 {
		t.Fatalf("expected a single finding, got: %v", missing)
	}
	if !strings.Contains(missing[0], `"user"`) || !strings.Contains(missing[0], `"usuario"`) {
		t.Fatalf("expected the finding to name both identifiers, got: %v", missing)
	}
}

func TestCheckJWTAuthSchemaMissingEmailField(t *testing.T) {
	entity := validUserEntity()
	entity.Fields = entity.Fields[:1]

	missing, _ := checkJWTAuthSchema([]*nemgen.Entity{entity})
	if !containsSubstring(missing, "email field") {
		t.Fatalf("expected an email field finding, got: %v", missing)
	}
}

func TestCheckJWTAuthSchemaMissingEmailIndex(t *testing.T) {
	entity := validUserEntity()
	entity.TypeConfig.Standalone.Indexes = entity.TypeConfig.Standalone.Indexes[:1]

	missing, _ := checkJWTAuthSchema([]*nemgen.Entity{entity})
	if !containsSubstring(missing, "index") {
		t.Fatalf("expected an index finding, got: %v", missing)
	}
}

func TestCheckJWTAuthSchemaCompositeIndexRejectedDatetimeIgnored(t *testing.T) {
	composite := validUserEntity()
	composite.TypeConfig.Standalone.Indexes[1].Fields = []*nemgen.IndexField{
		{FieldUuid: testEmailFieldUUID},
		{FieldUuid: testIDFieldUUID},
	}
	if missing, _ := checkJWTAuthSchema([]*nemgen.Entity{composite}); len(missing) == 0 {
		t.Fatal("expected a composite index over two usable fields to be rejected")
	}

	withDatetime := validUserEntity()
	withDatetime.TypeConfig.Standalone.Indexes[1].Fields = []*nemgen.IndexField{
		{FieldUuid: testEmailFieldUUID},
		{FieldUuid: testCreatedFieldUUID},
	}
	if missing, _ := checkJWTAuthSchema([]*nemgen.Entity{withDatetime}); len(missing) != 0 {
		t.Fatalf("expected datetime index members to be ignored, got: %v", missing)
	}
}

func TestCheckJWTAuthSchemaMissingPasswordOnlyWarns(t *testing.T) {
	entity := validUserEntity()
	entity.Fields = []*nemgen.Field{
		{Uuid: testIDFieldUUID, Identifier: "id", Type: nemgen.FieldType_FIELD_TYPE_UUID, Key: true},
		{Uuid: testEmailFieldUUID, Identifier: "email", Type: nemgen.FieldType_FIELD_TYPE_VARCHAR},
	}

	missing, warnings := checkJWTAuthSchema([]*nemgen.Entity{entity})
	if len(missing) != 0 {
		t.Fatalf("a missing password must not block generation, got: %v", missing)
	}
	if !containsSubstring(warnings, "password") {
		t.Fatalf("expected a password warning, got: %v", warnings)
	}
}

// The check must not fire for the auth modes that carry no schema dependency —
// and must not fetch the project version for them either, which is why the
// nil-receiver call below is safe.
func TestValidateJWTAuthRequirementsSkipsNonJWTAuth(t *testing.T) {
	var impl *Implementation
	for _, auth := range []interface{}{"disabled", "keycloak", "", nil} {
		configErr, warnings, err := impl.ValidateJWTAuthRequirements("pv-uuid", map[string]interface{}{"auth": auth})
		if err != nil || configErr != nil || len(warnings) != 0 {
			t.Fatalf("expected no findings for auth=%v, got err=%v configErr=%v warnings=%v", auth, err, configErr, warnings)
		}
	}
}

func containsSubstring(values []string, substr string) bool {
	for _, v := range values {
		if strings.Contains(v, substr) {
			return true
		}
	}
	return false
}
