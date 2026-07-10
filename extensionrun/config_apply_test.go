package extensionrun

import (
	"errors"
	"reflect"
	"testing"

	extensiongen "github.com/nuzur/extension-sdk/idl/gen"
	nemgen "github.com/nuzur/nem/idl/gen"
)

// stringField and friends build config-entity fields without UUID types, so the
// resolver never needs a live backend.
func scalarField(id string, t extensiongen.ExtensionInputType, required bool) *extensiongen.ExtensionInputField {
	return &extensiongen.ExtensionInputField{Identifier: id, Type: t, Required: required}
}

func enumField(id string, allowMultiple bool, required bool, options ...string) *extensiongen.ExtensionInputField {
	opts := make([]*extensiongen.ExtensionInputEnumOption, 0, len(options))
	for _, o := range options {
		opts = append(opts, &extensiongen.ExtensionInputEnumOption{Identifier: o})
	}
	return &extensiongen.ExtensionInputField{
		Identifier: id,
		Type:       extensiongen.ExtensionInputType_EXTENSION_INPUT_TYPE_ENUM,
		Required:   required,
		TypeConfig: &extensiongen.ExtensionInputTypeConfig{
			Enum: &extensiongen.ExtensionInputTypeEnumConfig{AllowMultiple: allowMultiple, Options: opts},
		},
	}
}

func entityWith(fields ...*extensiongen.ExtensionInputField) *extensiongen.ExtensionConfigurationEntity {
	return &extensiongen.ExtensionConfigurationEntity{Fields: fields}
}

func fieldErrorSet(err error) map[string]bool {
	var ve *ConfigValidationError
	if !errors.As(err, &ve) {
		return nil
	}
	out := map[string]bool{}
	for _, f := range ve.Fields {
		out[f.Field] = true
	}
	return out
}

func TestBuildConfigFromJSON_CoercionAndTypes(t *testing.T) {
	i := &Implementation{}
	project := &nemgen.Project{}
	entity := entityWith(
		scalarField("module_name", extensiongen.ExtensionInputType_EXTENSION_INPUT_TYPE_STRING, true),
		scalarField("port", extensiongen.ExtensionInputType_EXTENSION_INPUT_TYPE_INTEGER, true),
		scalarField("enabled", extensiongen.ExtensionInputType_EXTENSION_INPUT_TYPE_BOOLEAN, false),
		enumField("driver", false, true, "mysql", "postgres"),
		enumField("features", true, false, "rest", "grpc"),
	)

	provided := map[string]interface{}{
		"module_name": "acme",
		"port":        float64(8080), // JSON numbers decode to float64
		"enabled":     "true",        // string coerced to bool
		"driver":      "postgres",
		"features":    []interface{}{"rest", "grpc"},
	}

	values, err := i.BuildConfigFromJSON(project, "pv-uuid", entity, provided, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if values["module_name"] != "acme" {
		t.Errorf("module_name = %v, want acme", values["module_name"])
	}
	if values["port"] != "8080" { // numbers are stored as strings, mirroring the interactive flow
		t.Errorf("port = %v (%T), want string 8080", values["port"], values["port"])
	}
	if values["enabled"] != true {
		t.Errorf("enabled = %v (%T), want bool true", values["enabled"], values["enabled"])
	}
	if values["driver"] != "postgres" {
		t.Errorf("driver = %v, want postgres", values["driver"])
	}
	if got, want := values["features"], []string{"rest", "grpc"}; !reflect.DeepEqual(got, want) {
		t.Errorf("features = %#v, want %#v", got, want)
	}
}

func TestBuildConfigFromJSON_MergesOverLastConfig(t *testing.T) {
	i := &Implementation{}
	entity := entityWith(
		scalarField("module_name", extensiongen.ExtensionInputType_EXTENSION_INPUT_TYPE_STRING, true),
		enumField("driver", false, true, "mysql", "postgres"),
	)
	last := map[string]interface{}{"module_name": "old", "driver": "mysql"}
	provided := map[string]interface{}{"driver": "postgres"} // override just one field

	values, err := i.BuildConfigFromJSON(&nemgen.Project{}, "pv", entity, provided, last)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if values["module_name"] != "old" {
		t.Errorf("module_name = %v, want old (from last config)", values["module_name"])
	}
	if values["driver"] != "postgres" {
		t.Errorf("driver = %v, want postgres (overridden)", values["driver"])
	}
}

func TestBuildConfigFromJSON_Errors(t *testing.T) {
	i := &Implementation{}
	entity := entityWith(
		scalarField("module_name", extensiongen.ExtensionInputType_EXTENSION_INPUT_TYPE_STRING, true),
		scalarField("port", extensiongen.ExtensionInputType_EXTENSION_INPUT_TYPE_INTEGER, false),
		enumField("driver", false, true, "mysql", "postgres"),
	)

	provided := map[string]interface{}{
		// module_name missing -> required error
		"port":   "not-a-number", // invalid integer
		"driver": "oracle",       // not an allowed option
		"bogus":  "x",            // unknown field
	}

	_, err := i.BuildConfigFromJSON(&nemgen.Project{}, "pv", entity, provided, nil)
	if err == nil {
		t.Fatal("expected validation error, got nil")
	}
	got := fieldErrorSet(err)
	for _, want := range []string{"module_name", "port", "driver", "bogus"} {
		if !got[want] {
			t.Errorf("expected an error for field %q; got errors for %v", want, got)
		}
	}
}
