package app

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
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

// resolveCodegenIdentity runs the identifier half of runDeploy's config assembly and
// returns the config the GENERATOR ends up seeing — provided merged over the saved
// config, which is what BuildConfigFromJSON does — plus the overrides deploy reports.
func resolveCodegenIdentity(t *testing.T, flag, projectName string, provided, lastConfig map[string]interface{}) (map[string]interface{}, []string) {
	t.Helper()
	if provided == nil {
		provided = map[string]interface{}{}
	}
	renamed := applyCodegenIdentity(provided, lastConfig, flag)
	applyCodegenDefaults(goCodeGenEntity(), provided, lastConfig, sanitizeDBName(firstNonEmpty(flag, projectName)))
	effective := map[string]interface{}{}
	for k, v := range lastConfig {
		effective[k] = v
	}
	for k, v := range provided {
		if v != nil {
			effective[k] = v
		}
	}
	return effective, renamed
}

// --identifier's help says it names "the generated root folder/go module", and on any
// project that had run the generator once it did not: the identifier reached the
// generator only as a default for a MISSING field, so the saved config won. Deploying
// one project twice on one box (`--identifier terroirvpg` after a `terroirmy` deploy)
// produced a workspace named for the flag holding an app named for the saved config —
// two apps building the same go module and exporting the same gRPC service.
func TestApplyCodegenIdentity(t *testing.T) {
	saved := func() map[string]interface{} {
		return map[string]interface{}{"identifier": "terroirmy", "go_module": "github.com/nuzur/terroirmy"}
	}
	for _, tc := range []struct {
		name        string
		flag        string
		project     string
		provided    map[string]interface{}
		lastConfig  map[string]interface{}
		wantID      string
		wantModule  string
		wantRenamed []string
	}{
		{
			// No saved config: unchanged — the flag already reached the generator
			// through the defaults, and it still does.
			name: "no saved config, flag names the app",
			flag: "terroirvpg", project: "Terroir Coffee",
			wantID: "terroirvpg", wantModule: "terroirvpg",
		},
		{
			name:    "no saved config, no flag falls back to the project name",
			project: "Terroir Coffee",
			wantID:  "terroir_coffee", wantModule: "terroir_coffee",
		},
		{
			// A bare re-deploy states nothing, so the saved config keeps deciding —
			// that is what makes re-running the same deploy reproducible.
			name:    "saved config, no flag: saved wins",
			project: "Terroir Coffee", lastConfig: saved(),
			wantID: "terroirmy", wantModule: "github.com/nuzur/terroirmy",
		},
		{
			// The reported case.
			name: "saved config + explicit flag: the flag wins",
			flag: "terroirvpg", project: "Terroir Coffee", lastConfig: saved(),
			wantID: "terroirvpg", wantModule: "github.com/nuzur/terroirvpg",
			wantRenamed: []string{"identifier=terroirvpg", "go_module=github.com/nuzur/terroirvpg"},
		},
		{
			name: "a bare saved module is rebased too",
			flag: "terroirvpg", lastConfig: map[string]interface{}{"identifier": "terroirmy", "go_module": "terroirmy"},
			wantID: "terroirvpg", wantModule: "terroirvpg",
			wantRenamed: []string{"identifier=terroirvpg", "go_module=terroirvpg"},
		},
		{
			// A module path that was never derived from the identifier is somebody's
			// deliberate choice; renaming this deployment is not a reason to rewrite it.
			name: "a deliberately-named module is left alone",
			flag: "terroirvpg", lastConfig: map[string]interface{}{"identifier": "terroirmy", "go_module": "github.com/acme/coffee-api"},
			wantID: "terroirvpg", wantModule: "github.com/acme/coffee-api",
			wantRenamed: []string{"identifier=terroirvpg"},
		},
		{
			// The generator's own field, stated at the generator's level of detail,
			// still wins over the coarser deploy flag.
			name: "a codegen block naming identifier stays authoritative",
			flag: "terroirvpg",
			provided: map[string]interface{}{
				"identifier": "fromcodegen", "go_module": "github.com/acme/fromcodegen",
			},
			lastConfig: saved(),
			wantID:     "fromcodegen", wantModule: "github.com/acme/fromcodegen",
		},
		{
			// Same identifier as the saved one: nothing changes and nothing is announced.
			name: "flag matching the saved identifier is silent",
			flag: "terroirmy", lastConfig: saved(),
			wantID: "terroirmy", wantModule: "github.com/nuzur/terroirmy",
		},
		{
			// The generator's identifier is sanitized the same way the derived default
			// is, so the module path stays a legal one.
			name: "the flag is sanitized",
			flag: "Terroir-VPG", lastConfig: saved(),
			wantID: "terroir_vpg", wantModule: "github.com/nuzur/terroir_vpg",
			wantRenamed: []string{"identifier=terroir_vpg", "go_module=github.com/nuzur/terroir_vpg"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			effective, renamed := resolveCodegenIdentity(t, tc.flag, tc.project, tc.provided, tc.lastConfig)
			if got := effective["identifier"]; got != tc.wantID {
				t.Errorf("identifier = %v, want %v", got, tc.wantID)
			}
			if got := effective["go_module"]; got != tc.wantModule {
				t.Errorf("go_module = %v, want %v", got, tc.wantModule)
			}
			if !reflect.DeepEqual(renamed, tc.wantRenamed) {
				t.Errorf("reported overrides = %v, want %v", renamed, tc.wantRenamed)
			}
		})
	}
}

// Only what the generator is told changes. The deployment identifier — which names
// the database, the service and the config on the box, and which multi-project
// co-tenancy dedupes deployment records on — is derived separately and already
// preferred the flag; a fix that moved it would make a re-deploy miss its own record.
func TestApplyCodegenIdentityLeavesTheDeploymentIdentifierAlone(t *testing.T) {
	lastConfig := map[string]interface{}{"identifier": "terroirmy", "go_module": "github.com/nuzur/terroirmy"}
	effective, _ := resolveCodegenIdentity(t, "terroirvpg", "Terroir Coffee", nil, lastConfig)

	// The record/database/service name, resolved from the config this deploy built.
	if got := planIdentifier("terroirvpg", effective, "Terroir Coffee"); got != "terroirvpg" {
		t.Errorf("deployment identifier = %q, want terroirvpg", got)
	}
	// And a re-deploy that omits the flag still lands on the same record, because the
	// saved config now carries the name the flagged deploy generated under.
	if got := planIdentifier("", effective, "Terroir Coffee"); got != "terroirvpg" {
		t.Errorf("bare re-deploy identifier = %q, want terroirvpg", got)
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

// TestResolveCodegenDomains covers the project config becoming the durable home
// for the three hostnames, in both directions.
func TestResolveCodegenDomains(t *testing.T) {
	t.Run("writes what this deploy resolved", func(t *testing.T) {
		s := &deploySettings{Domain: "api.example.com", GRPCDomain: "grpc.example.com", AuthDomain: "auth.example.com"}
		provided := map[string]interface{}{}
		if adopted := resolveCodegenDomains(s, provided, nil); len(adopted) != 0 {
			t.Errorf("nothing was adopted from the config, got %v", adopted)
		}
		for field, want := range map[string]string{
			"domain": "api.example.com", "grpc_domain": "grpc.example.com", "auth_domain": "auth.example.com",
		} {
			if got := provided[field]; got != want {
				t.Errorf("%s: got %v, want %q", field, got, want)
			}
		}
	})

	t.Run("adopts what the config holds", func(t *testing.T) {
		s := &deploySettings{}
		last := map[string]interface{}{"domain": "api.saved.com", "auth_domain": "auth.saved.com"}
		adopted := resolveCodegenDomains(s, map[string]interface{}{}, last)
		if s.Domain != "api.saved.com" || s.AuthDomain != "auth.saved.com" {
			t.Errorf("hostnames not adopted: %+v", s)
		}
		if s.GRPCDomain != "" {
			t.Errorf("nothing saved for grpc_domain, so nothing to adopt; got %q", s.GRPCDomain)
		}
		if len(adopted) != 2 {
			t.Errorf("both adoptions should be reported, got %v", adopted)
		}
	})

	t.Run("an explicit hostname wins over the saved one", func(t *testing.T) {
		s := &deploySettings{Domain: "api.new.com"}
		provided := map[string]interface{}{}
		resolveCodegenDomains(s, provided, map[string]interface{}{"domain": "api.old.com"})
		if s.Domain != "api.new.com" {
			t.Errorf("the flag must win, got %q", s.Domain)
		}
		if provided["domain"] != "api.new.com" {
			t.Errorf("the new value must be written back, got %v", provided["domain"])
		}
	})

	// The one that would be silent and destructive. An omitted flag means "I did
	// not say", not "remove it" — writing "" would clear the saved hostname, and
	// the next deploy would then resolve nothing and delete the live Ingress.
	t.Run("an unstated hostname never clears the saved one", func(t *testing.T) {
		s := &deploySettings{}
		provided := map[string]interface{}{}
		resolveCodegenDomains(s, provided, map[string]interface{}{"domain": "api.saved.com"})
		if _, written := provided["grpc_domain"]; written {
			t.Errorf("an unset hostname must not be written at all, got %v", provided["grpc_domain"])
		}
		if v, written := provided["domain"]; written && v == "" {
			t.Error("writing an empty domain would erase the saved one")
		}
	})
}

// TestCheckDistinctDomains: two Ingresses on one host is a silent misroute, not
// an error helm or the cluster will report.
func TestCheckDistinctDomains(t *testing.T) {
	t.Run("distinct is fine", func(t *testing.T) {
		s := &deploySettings{Domain: "api.example.com", GRPCDomain: "grpc.example.com", AuthDomain: "auth.example.com"}
		if err := checkDistinctDomains(s); err != nil {
			t.Errorf("three different hostnames must be accepted: %v", err)
		}
	})

	t.Run("empties do not collide with each other", func(t *testing.T) {
		if err := checkDistinctDomains(&deploySettings{}); err != nil {
			t.Errorf("no hostnames at all is not a collision: %v", err)
		}
		if err := checkDistinctDomains(&deploySettings{GRPCDomain: "api.example.com"}); err != nil {
			t.Errorf("one hostname is not a collision: %v", err)
		}
	})

	// The live case: moving an app's front door from HTTP to gRPC leaves the old
	// value saved in the config, so both point at the same host.
	t.Run("moving a host between fields is refused", func(t *testing.T) {
		s := &deploySettings{Domain: "api.example.com", GRPCDomain: "api.example.com"}
		err := checkDistinctDomains(s)
		if err == nil {
			t.Fatal("the same host on two Ingresses must be refused")
		}
		for _, want := range []string{"domain", "grpc_domain", "api.example.com"} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("the error must name %q so the user knows what to clear: %v", want, err)
			}
		}
	})

	t.Run("case and spacing do not hide a collision", func(t *testing.T) {
		s := &deploySettings{Domain: " API.example.com ", AuthDomain: "api.example.com"}
		if err := checkDistinctDomains(s); err == nil {
			t.Error("hostnames are case-insensitive; this is the same host")
		}
	})
}

// TestCheckHandMaintainedChart guards a chart nobody generated.
//
// The k8s provider force-enables `helm` whatever the saved config says and then
// persists the result, so one deploy against a hand-maintained chart would
// rewrite it from templates and make that permanent. For sfapi that drops
// cert-manager.io/cluster-issuer and the tls block from a production gRPC API —
// which does not degrade to plain HTTP, it stops serving gRPC entirely.
func TestCheckHandMaintainedChart(t *testing.T) {
	const marker = "# Code generated by nuzur go-code-gen. DO NOT EDIT.\n"

	write := func(t *testing.T, dir string, files map[string]string) string {
		t.Helper()
		chart := filepath.Join(dir, ".helm", "sfapi")
		if err := os.MkdirAll(chart, 0755); err != nil {
			t.Fatal(err)
		}
		for name, body := range files {
			if err := os.WriteFile(filepath.Join(chart, name), []byte(body), 0644); err != nil {
				t.Fatal(err)
			}
		}
		return chart
	}

	t.Run("no chart is the normal first deploy", func(t *testing.T) {
		if err := checkHandMaintainedChart(filepath.Join(t.TempDir(), "nope")); err != nil {
			t.Errorf("a missing chart must not refuse: %v", err)
		}
	})

	t.Run("a generated chart is regenerated freely", func(t *testing.T) {
		chart := write(t, t.TempDir(), map[string]string{
			"Chart.yaml":  marker + "name: sfapi\n",
			"values.yaml": marker + "replicaCount: 1\n",
		})
		if err := checkHandMaintainedChart(chart); err != nil {
			t.Errorf("a generated chart is ours to rewrite: %v", err)
		}
	})

	t.Run("a hand-written chart is refused", func(t *testing.T) {
		chart := write(t, t.TempDir(), map[string]string{
			"Chart.yaml":  "apiVersion: v2\nname: sfapi\n",
			"values.yaml": "# Default values for sfapi.\ningress:\n  annotations:\n    cert-manager.io/cluster-issuer: lets-encrypt\n",
		})
		err := checkHandMaintainedChart(chart)
		if err == nil {
			t.Fatal("a hand-maintained chart must not be silently overwritten")
		}
		// The message has to name the way out, or the refusal is just a wall.
		if !strings.Contains(err.Error(), "--release-only") {
			t.Errorf("the refusal must say how to deploy it anyway: %v", err)
		}
	})

	t.Run("a chart taken over file-by-file is refused", func(t *testing.T) {
		// Stripping the marker is the documented way to take a generated file
		// over — the manifest then skips it forever. Doing that to values.yaml
		// means the chart is no longer wholly ours.
		chart := write(t, t.TempDir(), map[string]string{
			"Chart.yaml":  marker + "name: sfapi\n",
			"values.yaml": "replicaCount: 3\n",
		})
		if err := checkHandMaintainedChart(chart); err == nil {
			t.Error("an unmarked values.yaml means the user took it over")
		}
	})

	t.Run("unmarked .helmignore and NOTES.txt do not trigger it", func(t *testing.T) {
		// Both are unmarked even in a fully generated chart: generatedNotice has
		// no comment syntax for those extensions. Checking "any unmarked file"
		// would therefore refuse every chart we generate.
		dir := t.TempDir()
		chart := write(t, dir, map[string]string{
			"Chart.yaml":  marker + "name: sfapi\n",
			"values.yaml": marker + "replicaCount: 1\n",
			".helmignore": ".git\n",
		})
		if err := os.MkdirAll(filepath.Join(chart, "templates"), 0755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(chart, "templates", "NOTES.txt"), []byte("Released.\n"), 0644); err != nil {
			t.Fatal(err)
		}
		if err := checkHandMaintainedChart(chart); err != nil {
			t.Errorf("unmarked NOTES.txt/.helmignore are normal in a generated chart: %v", err)
		}
	})
}
