package app

import (
	"reflect"
	"testing"

	extensiongen "github.com/nuzur/extension-sdk/idl/gen"
)

// goCodeGenEntity mirrors the go-code-gen extension's config schema (required
// flags as the generator declares them), so these tests fail if deploy stops
// covering a field the generator insists on.
func goCodeGenEntity() *extensiongen.ExtensionConfigurationEntity {
	str := func(id string, required bool) *extensiongen.ExtensionInputField {
		return &extensiongen.ExtensionInputField{Identifier: id, Type: extensiongen.ExtensionInputType_EXTENSION_INPUT_TYPE_STRING, Required: required}
	}
	b := func(id string, required bool) *extensiongen.ExtensionInputField {
		return &extensiongen.ExtensionInputField{Identifier: id, Type: extensiongen.ExtensionInputType_EXTENSION_INPUT_TYPE_BOOLEAN, Required: required}
	}
	enum := func(id string, required bool, options ...string) *extensiongen.ExtensionInputField {
		opts := make([]*extensiongen.ExtensionInputEnumOption, 0, len(options))
		for _, o := range options {
			opts = append(opts, &extensiongen.ExtensionInputEnumOption{Identifier: o})
		}
		return &extensiongen.ExtensionInputField{
			Identifier: id,
			Type:       extensiongen.ExtensionInputType_EXTENSION_INPUT_TYPE_ENUM,
			Required:   required,
			TypeConfig: &extensiongen.ExtensionInputTypeConfig{Enum: &extensiongen.ExtensionInputTypeEnumConfig{Options: opts}},
		}
	}
	return &extensiongen.ExtensionConfigurationEntity{Fields: []*extensiongen.ExtensionInputField{
		str("identifier", true),
		str("go_module", true),
		enum("db", true, "mysql", "postgresql"),
		enum("events", true, "disabled", "kafka"),
		b("proto_enabled", true),
		b("grpc_server_enabled", true),
		b("rest_enabled", true),
		str("grpc_port", true),
		str("http_port", true),
		enum("auth", true, "disabled", "jwt", "keycloak"),
		b("dockerfile", true),
		b("helm", true),
		b("github_actions", true),
		str("entities_dir", false),
		b("custom_enabled", false),
		b("storage_enabled", false),
		str("object_store", false),
	}}
}

// The case that motivated this: a project created via the API has no last-used
// config, so a first deploy has to stand up a complete generator config from the
// deploy flags alone.
func TestApplyCodegenDefaults_FirstDeployCoversEveryRequiredField(t *testing.T) {
	entity := goCodeGenEntity()
	// what runDeploy puts in `provided` before defaults, for a plain
	// `deploy --host … --project …` with no --api/--auth
	provided := map[string]interface{}{
		"db":             "mysql",
		"custom_enabled": false,
		"dockerfile":     true,
	}

	applied := applyCodegenDefaults(entity, provided, nil, "meridian")
	if len(applied) == 0 {
		t.Fatal("expected defaults to be applied for a project with no saved config")
	}

	for _, field := range entity.Fields {
		if !field.Required {
			continue
		}
		if v, ok := provided[field.Identifier]; !ok || v == nil {
			t.Errorf("required field %q left unset — the first deploy would still fail validation", field.Identifier)
		}
	}

	if provided["identifier"] != "meridian" || provided["go_module"] != "meridian" {
		t.Errorf("identifier/go_module should come from the deploy identifier: %v / %v", provided["identifier"], provided["go_module"])
	}
	// The smallest working app: REST on, no gRPC/proto, no auth.
	if provided["rest_enabled"] != true || provided["grpc_server_enabled"] != false || provided["proto_enabled"] != false {
		t.Errorf("expected a REST-only default surface, got %v", provided)
	}
	if provided["auth"] != "disabled" || provided["events"] != "disabled" {
		t.Errorf("expected auth/events disabled by default, got %v / %v", provided["auth"], provided["events"])
	}
	// Optional fields stay out of the way — the generator's own defaults apply.
	if _, ok := provided["entities_dir"]; ok {
		t.Error("optional fields must not be defaulted by deploy")
	}
}

// Defaults are a last resort: anything the caller or the project already says
// wins, so a re-deploy is byte-identical to the deploy before it.
func TestApplyCodegenDefaults_NeverOverridesProvidedOrSaved(t *testing.T) {
	entity := goCodeGenEntity()
	provided := map[string]interface{}{
		"db":            "postgresql",
		"rest_enabled":  false,
		"proto_enabled": true,
		"auth":          "jwt",
	}
	lastConfig := map[string]interface{}{
		"identifier": "saved_app",
		"go_module":  "github.com/acme/saved_app",
		"http_port":  "9999",
	}

	applied := applyCodegenDefaults(entity, provided, lastConfig, "flag_identifier")

	if provided["db"] != "postgresql" || provided["auth"] != "jwt" || provided["rest_enabled"] != false || provided["proto_enabled"] != true {
		t.Errorf("explicit values were overwritten: %v", provided)
	}
	for _, key := range []string{"identifier", "go_module", "http_port"} {
		if _, ok := provided[key]; ok {
			t.Errorf("%q is in the saved config — deploy must not default it", key)
		}
	}
	// Only the genuinely-missing required fields get filled.
	want := []string{"events=disabled", "grpc_server_enabled=false", "grpc_port=6009", "dockerfile=true", "helm=false", "github_actions=false"}
	if !reflect.DeepEqual(applied, want) {
		t.Errorf("applied defaults = %v, want %v", applied, want)
	}
}

// A saved config that predates a newly-required generator field is topped up
// rather than left to fail validation.
func TestApplyCodegenDefaults_TopsUpSavedConfig(t *testing.T) {
	entity := &extensiongen.ExtensionConfigurationEntity{Fields: []*extensiongen.ExtensionInputField{
		{Identifier: "identifier", Type: extensiongen.ExtensionInputType_EXTENSION_INPUT_TYPE_STRING, Required: true},
		{Identifier: "events", Type: extensiongen.ExtensionInputType_EXTENSION_INPUT_TYPE_STRING, Required: true},
	}}
	provided := map[string]interface{}{}
	lastConfig := map[string]interface{}{"identifier": "old_app"}

	applied := applyCodegenDefaults(entity, provided, lastConfig, "new_app")
	if !reflect.DeepEqual(applied, []string{"events=disabled"}) {
		t.Fatalf("applied = %v, want only the missing field", applied)
	}
}

// A field deploy has no opinion about is left alone: better a precise
// "required field is missing" than a made-up value.
func TestApplyCodegenDefaults_UnknownRequiredFieldLeftAlone(t *testing.T) {
	entity := &extensiongen.ExtensionConfigurationEntity{Fields: []*extensiongen.ExtensionInputField{
		{Identifier: "brand_new_thing", Type: extensiongen.ExtensionInputType_EXTENSION_INPUT_TYPE_STRING, Required: true},
	}}
	provided := map[string]interface{}{}
	if applied := applyCodegenDefaults(entity, provided, nil, "app"); len(applied) != 0 {
		t.Fatalf("applied = %v, want none", applied)
	}
	if len(provided) != 0 {
		t.Fatalf("provided = %v, want untouched", provided)
	}
}

// An explicit JSON null in a `codegen` block fails the required check, so it
// counts as missing here too.
func TestApplyCodegenDefaults_ExplicitNullIsMissing(t *testing.T) {
	entity := &extensiongen.ExtensionConfigurationEntity{Fields: []*extensiongen.ExtensionInputField{
		{Identifier: "go_module", Type: extensiongen.ExtensionInputType_EXTENSION_INPUT_TYPE_STRING, Required: true},
	}}
	provided := map[string]interface{}{"go_module": nil}
	if applied := applyCodegenDefaults(entity, provided, nil, "app"); len(applied) != 1 {
		t.Fatalf("applied = %v, want the null to be filled", applied)
	}
	if provided["go_module"] != "app" {
		t.Fatalf("go_module = %v, want the derived default", provided["go_module"])
	}
}
