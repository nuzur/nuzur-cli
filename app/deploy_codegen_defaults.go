package app

import (
	"fmt"
	"path"
	"sort"
	"strings"

	extensiongen "github.com/nuzur/extension-sdk/idl/gen"
)

// Deploy reuses the project's last-used go-code-gen config, which a project
// created through the API/MCP (or the web app, before anyone ran the generator)
// simply does not have. Deploy's own flags cover only part of the generator's
// required surface — db, dockerfile, custom, and (only when passed) api/auth —
// so the first deploy of a brand-new project used to die in config validation
// with "identifier: required field is missing; go_module: ...", which says
// nothing about what to do next.
//
// The values below are what deploy fills in for a required generator field that
// nothing else supplies. They are deliberately the smallest working app: a REST
// API, no auth, no events, no helm/CI scaffolding — the same shape the deploy
// output already describes (curl the REST base path). Everything here is a
// fallback of last resort: an explicit flag, a --deploy-config `codegen` block,
// a --gen-config file, and the project's saved config all win over it.
const (
	// Internal ports the generated app binds. Cosmetic for a deploy — the box
	// allocates the real pair and writes them into prod.yaml, which deep-merges
	// over the generated base.yaml — but the generator requires a value.
	defaultCodegenGRPCPort = "6009"
	defaultCodegenHTTPPort = "8080"
)

// k8sRequiredCodegen are the generator options the k8s path cannot work
// without, and the reason each is required.
//
// These are REQUIREMENTS, not defaults: applyCodegenDefaults only fills fields
// that are both required by the generator and missing, and none of these is
// either — `helm` and `github_actions` default to false and a project's saved
// config very likely already says so. Left at false there is simply nothing to
// release: no chart to install, or no image to install with it.
var k8sRequiredCodegen = map[string]string{
	"helm":           "the k8s deploy installs the chart the generator emits",
	"github_actions": "the image the chart runs is built by the generated workflow",
	"dockerfile":     "the workflow builds from the generated Dockerfile",
}

// applyK8sCodegenRequirements forces those options on, returning the ones it had
// to change so the deploy can say it overrode the project's saved config.
func applyK8sCodegenRequirements(provided, lastConfig map[string]interface{}) []string {
	if provided == nil {
		return nil
	}
	var forced []string
	for field := range k8sRequiredCodegen {
		if boolValue(provided, field) {
			continue
		}
		// Report only a real change: silent when the value was already true in
		// the saved config and simply absent from this run's explicit values.
		if _, set := provided[field]; !set && boolValue(lastConfig, field) {
			provided[field] = true
			continue
		}
		provided[field] = true
		forced = append(forced, field)
	}
	sort.Strings(forced)
	return forced
}

// deployCodegenDefaults is the fallback value per go-code-gen config field,
// keyed by field identifier. identifier drives the generated root folder and go
// module, so it is derived from the deploy (--identifier, else the project name).
//
// Keys absent from this map have no deploy-derived default: if such a field is
// required and unset, validation still fails — with the actionable message
// runDeploy wraps it in.
func deployCodegenDefaults(identifier string) map[string]interface{} {
	return map[string]interface{}{
		"identifier": identifier,
		// A module path with no dot in its first element is never fetched
		// remotely, which is exactly right for an app whose source lives in the
		// deploy workspace and is built on the box.
		"go_module":           identifier,
		"db":                  "mysql",
		"events":              "disabled",
		"auth":                "disabled",
		"proto_enabled":       false,
		"grpc_server_enabled": false,
		"rest_enabled":        true,
		"grpc_port":           defaultCodegenGRPCPort,
		"http_port":           defaultCodegenHTTPPort,
		"dockerfile":          true,
		"helm":                false,
		"github_actions":      false,
	}
}

// applyCodegenDefaults fills, in provided, every REQUIRED config field that
// neither the caller (flags / deploy-config / --gen-config) nor the project's
// last-used config supplies. It returns the "field=value" pairs it filled, in
// the extension's own field order, for the notice deploy prints.
//
// Only required fields are defaulted: an optional field left unset keeps
// whatever the generator itself defaults to, which is not deploy's call to make.
// Only missing fields are touched, so a re-deploy — or any explicitly supplied
// value — is never overridden, and a saved config that predates a newly-required
// generator field gets topped up instead of failing.
func applyCodegenDefaults(configEntity *extensiongen.ExtensionConfigurationEntity, provided, lastConfig map[string]interface{}, identifier string) []string {
	if configEntity == nil || provided == nil {
		return nil
	}
	defaults := deployCodegenDefaults(identifier)
	var applied []string
	for _, field := range configEntity.Fields {
		if field == nil || !field.Required {
			continue
		}
		// An explicit null is "missing" here for the same reason it is in
		// BuildConfigFromJSON: it fails the required check either way.
		if v, ok := provided[field.Identifier]; ok && v != nil {
			continue
		}
		if v, ok := lastConfig[field.Identifier]; ok && v != nil {
			continue
		}
		def, ok := defaults[field.Identifier]
		if !ok {
			continue
		}
		provided[field.Identifier] = def
		applied = append(applied, fmt.Sprintf("%s=%v", field.Identifier, def))
	}
	return applied
}

// applyCodegenIdentity makes an explicitly passed --identifier name what its help
// says it names: the generated root folder and go module, not just the database,
// the service and the workspace directory. It returns the "field=value" pairs it
// changed relative to the saved config, for the notice deploy prints, and nothing
// at all when the identifier was not stated this run.
//
// The identifier only ever reached the generator through applyCodegenDefaults,
// which fills MISSING fields — and `provided` is the only thing that beats the
// project's saved config (BuildConfigFromJSON merges provided OVER lastConfig). So
// on any project that had run the generator once, --identifier moved the deployment
// record, the database and the workspace root while the generated module, root
// folder and gRPC service kept the name the previous deploy saved: the same project
// deployed twice on one box under two identifiers produced two apps building the
// same go module and exporting the same service name. That is the documented
// precedence backwards — an explicit flag is supposed to win over saved config.
//
// Only the GENERATOR's view changes here. The deployment identifier (planIdentifier)
// already preferred the flag, and co-tenancy dedupes deployment records on it, so
// nothing about which record a deploy matches moves.
//
// A bare re-deploy passes "" and keeps every saved value, which is what makes
// re-running the same deploy reproducible.
func applyCodegenIdentity(provided, lastConfig map[string]interface{}, identifier string) []string {
	if provided == nil {
		return nil
	}
	id := strings.TrimSpace(identifier)
	if id == "" {
		return nil
	}
	// Same derivation applyCodegenDefaults uses, so the flag names the generated
	// module identically whether or not the project has a saved config.
	id = sanitizeDBName(id)

	// A `codegen` block or --gen-config that names `identifier` outright stays
	// authoritative: that is the generator's own field, stated at the generator's
	// level of detail, and it wins over the coarser deploy flag exactly as it does
	// for every other codegen key.
	if v, ok := provided["identifier"]; ok && v != nil {
		return nil
	}

	// Reported only when it actually overrides something the project had saved: on a
	// first deploy this writes the same value applyCodegenDefaults would derive, and
	// announcing an override of nothing is noise.
	var applied []string
	savedID := stringValue(lastConfig, "identifier", "")
	if savedID != "" && savedID != id {
		applied = append(applied, fmt.Sprintf("identifier=%s", id))
	}
	provided["identifier"] = id

	// go_module follows the identifier only when it was DERIVED from it — either the
	// bare identifier (deploy's own default) or `<prefix>/<identifier>`. A module path
	// chosen for unrelated reasons is left alone: renaming somebody's module because
	// they named this deployment differently is not what the flag promises. An absent
	// saved module is left to applyCodegenDefaults, which derives it from the same id.
	if v, ok := provided["go_module"]; ok && v != nil {
		return applied
	}
	savedModule := stringValue(lastConfig, "go_module", "")
	if savedID == "" || savedModule == "" || path.Base(savedModule) != savedID {
		return applied
	}
	module := id
	if prefix := path.Dir(savedModule); prefix != "." && prefix != "/" {
		module = prefix + "/" + id
	}
	if module != savedModule {
		applied = append(applied, fmt.Sprintf("go_module=%s", module))
	}
	provided["go_module"] = module
	return applied
}

// customStickinessNotice is the line deploy prints when the custom-application zone
// was decided by the project's SAVED generator config rather than by this run —
// "" when the user said something about it themselves (--custom, `custom` in a
// deploy-config, or custom_enabled in a codegen block), because then there is
// nothing to disclose.
//
// The stickiness is deliberate: omitting --custom used to regenerate the app with
// the zone off and silently drop every hand-written endpoint. But a setting that
// carries itself forward invisibly is only half-fixed — nothing in the output said
// the zone had been preserved, so the behaviour stayed undiscoverable and a user who
// wanted it OFF had no idea what to pass.
func customStickinessNotice(flagCustom *bool, provided, lastConfig map[string]interface{}) string {
	if flagCustom != nil {
		return ""
	}
	if v, ok := provided["custom_enabled"]; ok && v != nil {
		return ""
	}
	saved, ok := lastConfig["custom_enabled"]
	if !ok || saved == nil {
		return ""
	}
	enabled, ok := configBool(saved)
	if !ok {
		// A value of an unexpected shape: say nothing rather than announce a state
		// that may not be the one the generator resolves.
		return ""
	}
	if enabled {
		return "Custom endpoints: enabled (kept from the previous deploy — pass --custom=false to disable)"
	}
	return "Custom endpoints: disabled (kept from the previous deploy — pass --custom to enable)"
}

// configBool reads a saved config's boolean. Saved configs round-trip through JSON,
// so a bool is what it normally is; the string forms are accepted because a
// hand-written --gen-config file can carry them and the generator coerces them too.
func configBool(v interface{}) (bool, bool) {
	switch t := v.(type) {
	case bool:
		return t, true
	case string:
		switch strings.ToLower(strings.TrimSpace(t)) {
		case "true":
			return true, true
		case "false":
			return false, true
		}
	}
	return false, false
}

// codegenDomainFields pairs each deploy setting with the go-code-gen config
// field that is its durable home.
var codegenDomainFields = []struct {
	field string
	get   func(*deploySettings) string
	set   func(*deploySettings, string)
}{
	{"domain", func(s *deploySettings) string { return s.Domain }, func(s *deploySettings, v string) { s.Domain = v }},
	{"grpc_domain", func(s *deploySettings) string { return s.GRPCDomain }, func(s *deploySettings, v string) { s.GRPCDomain = v }},
	{"auth_domain", func(s *deploySettings) string { return s.AuthDomain }, func(s *deploySettings, v string) { s.AuthDomain = v }},
}

// resolveCodegenDomains carries the three hostnames between this deploy and the
// project's saved generator config, in BOTH directions.
//
// Reading: a hostname nothing else supplied is taken from the saved config. That
// is the point of putting domains there — they stop living only in a flag and a
// local deployment record, and survive a new machine.
//
// Writing: a hostname this deploy resolved is written back, so the config becomes
// the durable home rather than a stale copy, and so the values.yaml the generator
// commits carries the REAL host instead of a placeholder. Anything reaching here
// has already been through flag / --deploy-config / deployment-record resolution,
// so the config is consulted last and overridden by all three.
//
// An empty hostname is never written. Writing one would clear a domain the config
// already holds — the same silent removal guardIngressRemoval exists to stop, just
// one layer earlier and with no cluster to notice it. Removing a hostname is done
// by editing the project config, not by omitting a flag.
//
// Returns the "field=value" pairs it adopted FROM the config, for the deploy to
// report; values written back are not news, since the user just supplied them.
func resolveCodegenDomains(s *deploySettings, provided, lastConfig map[string]interface{}) []string {
	if s == nil || provided == nil {
		return nil
	}
	var adopted []string
	for _, d := range codegenDomainFields {
		host := strings.TrimSpace(d.get(s))
		if host == "" {
			if saved := strings.TrimSpace(stringValue(lastConfig, d.field, "")); saved != "" {
				d.set(s, saved)
				adopted = append(adopted, d.field+"="+saved)
			}
			continue
		}
		provided[d.field] = host
	}
	return adopted
}

// checkDistinctDomains refuses two hostnames that are the same.
//
// Each of the three renders its OWN Ingress object, and ingress-nginx MERGES
// Ingresses that share a host. Which object's annotations survive the merge then
// depends on creation age, so the same config produces different routing
// depending on install order — and the failure is silent: helm installs, both
// objects exist, and gRPC either works or does not depending on which one was
// created first.
//
// It is reachable in one step now that domains are saved: pointing --grpc-domain
// at the host already saved as `domain` leaves both set, which is exactly what
// moving an app from an HTTP front door to a gRPC one looks like. Clearing the
// old one is a config edit, so this says which field to clear rather than
// guessing which the user meant.
func checkDistinctDomains(s *deploySettings) error {
	if s == nil {
		return nil
	}
	seen := map[string]string{}
	for _, d := range codegenDomainFields {
		host := strings.ToLower(strings.TrimSpace(d.get(s)))
		if host == "" {
			continue
		}
		if first, dup := seen[host]; dup {
			return fmt.Errorf(
				"%s and %s are both %s, and each renders its own Ingress — ingress-nginx merges Ingresses sharing a host, so which annotations apply would depend on which object is older. Give them different hostnames, or clear the one you no longer serve in the project's go-code-gen config (a deploy never clears a saved hostname, so omitting the flag will not do it)",
				first, d.field, host)
		}
		seen[host] = d.field
	}
	return nil
}
