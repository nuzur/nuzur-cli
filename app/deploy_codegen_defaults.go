package app

import (
	"fmt"

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
