package extensionrun

import (
	extensiongen "github.com/nuzur/extension-sdk/idl/gen"
	nemgen "github.com/nuzur/nem/idl/gen"
)

// The types in this file form a STABLE, machine-readable contract emitted by the
// `describe` command and intended for consumption by AI agents and MCP tooling.
// Treat changes to the JSON tags / shapes as breaking. See docs/agent-usage.md.

// ConfigSchema fully describes what an extension needs in order to run against a
// specific project version: the list of configuration fields, their types, and —
// crucially — the concrete set of allowed values for uuid/enum fields, so a
// caller never has to guess an entity/connection/store UUID.
type ConfigSchema struct {
	Extension      SchemaExtension        `json:"extension"`
	Project        SchemaRef              `json:"project"`
	ProjectVersion SchemaRef              `json:"project_version"`
	Fields         []ConfigFieldSchema    `json:"fields"`
	LastUsedConfig map[string]interface{} `json:"last_used_config,omitempty"`
}

// SchemaExtension identifies the resolved extension + version the schema is for.
type SchemaExtension struct {
	Identifier  string `json:"identifier"`
	DisplayName string `json:"display_name,omitempty"`
	Version     string `json:"version,omitempty"`
	VersionUUID string `json:"version_uuid,omitempty"`
}

// SchemaRef is a uuid + human identifier pair for a project or project version.
type SchemaRef struct {
	Uuid       string `json:"uuid"`
	Identifier string `json:"identifier,omitempty"`
}

// ConfigFieldSchema describes a single configuration field. Arrays and
// multi-select enums are represented as the element `type` plus `multiple:true`
// rather than a distinct "array" type, so callers handle them uniformly.
type ConfigFieldSchema struct {
	Identifier  string         `json:"identifier"`
	DisplayName string         `json:"display_name,omitempty"`
	Description string         `json:"description,omitempty"`
	Type        string         `json:"type"` // string|integer|float|boolean|uuid|enum|date|datetime
	Required    bool           `json:"required"`
	Multiple    bool           `json:"multiple,omitempty"` // true for arrays / multi-select enums
	Options     []ConfigOption `json:"options,omitempty"`  // allowed values for uuid/enum fields
}

// ConfigOption is one allowed value for a uuid or enum field. `value` is what the
// caller must place in the config; `label` is a human-friendly name for it.
type ConfigOption struct {
	Value string `json:"value"`
	Label string `json:"label,omitempty"`
}

// typeName maps an extension input type to its stable schema type string.
func typeName(t extensiongen.ExtensionInputType) string {
	switch t {
	case extensiongen.ExtensionInputType_EXTENSION_INPUT_TYPE_UUID:
		return "uuid"
	case extensiongen.ExtensionInputType_EXTENSION_INPUT_TYPE_INTEGER:
		return "integer"
	case extensiongen.ExtensionInputType_EXTENSION_INPUT_TYPE_FLOAT:
		return "float"
	case extensiongen.ExtensionInputType_EXTENSION_INPUT_TYPE_BOOLEAN:
		return "boolean"
	case extensiongen.ExtensionInputType_EXTENSION_INPUT_TYPE_STRING:
		return "string"
	case extensiongen.ExtensionInputType_EXTENSION_INPUT_TYPE_DATE:
		return "date"
	case extensiongen.ExtensionInputType_EXTENSION_INPUT_TYPE_DATETIME:
		return "datetime"
	case extensiongen.ExtensionInputType_EXTENSION_INPUT_TYPE_ENUM:
		return "enum"
	default:
		return "string"
	}
}

// ConfigResolver resolves and caches the concrete option lists (entities,
// connections, object stores, local agents) for a project version, so uuid/enum
// fields can be described and validated without re-fetching per field. It is
// shared by `describe` (to list options), the non-interactive config apply (to
// validate membership) and the interactive prompts (to offer choices).
type ConfigResolver struct {
	er                 *Implementation
	project            *nemgen.Project
	projectVersionUUID string

	entities     []ConfigOption
	entitiesDone bool
	connections  []ConfigOption
	connDone     bool
	stores       []ConfigOption
	storesDone   bool
	agents       []*nemgen.LocalAgent
	agentsDone   bool
}

func (i *Implementation) NewConfigResolver(project *nemgen.Project, projectVersionUUID string) *ConfigResolver {
	return &ConfigResolver{
		er:                 i,
		project:            project,
		projectVersionUUID: projectVersionUUID,
	}
}

// UUIDFieldEntityType reads the entity type declared on a uuid field. The
// sql-*-local extensions omit type_config entirely, so a missing one is normal
// rather than a reason to skip resolving options — see EffectiveEntityType.
func UUIDFieldEntityType(field *extensiongen.ExtensionInputField) extensiongen.EntityType {
	if tc := field.GetTypeConfig(); tc != nil && tc.GetUuid() != nil {
		return tc.GetUuid().GetEntityType()
	}
	return extensiongen.EntityType_ENTITY_TYPE_INVALID
}

// EffectiveEntityType supplies the entity type for uuid fields whose extension
// version was registered without one. The sql-*-local extensions declare
// local_agent and local_agent_connection as bare uuid fields, so on their own
// they would prompt for a hand-typed UUID instead of offering the agents this
// machine can actually reach. Those field identifiers are already a contract
// across the CLI and the web UI, so keying off them is safe.
//
// TODO: remove once those extension versions declare entity types
// LOCAL_AGENT / LOCAL_AGENT_CONNECTION themselves.
func EffectiveEntityType(declared extensiongen.EntityType, fieldIdentifier string) extensiongen.EntityType {
	if declared != extensiongen.EntityType_ENTITY_TYPE_INVALID {
		return declared
	}
	switch fieldIdentifier {
	case "local_agent":
		return extensiongen.EntityType_ENTITY_TYPE_LOCAL_AGENT
	case "local_agent_connection":
		return extensiongen.EntityType_ENTITY_TYPE_LOCAL_AGENT_CONNECTION
	}
	return declared
}

// OptionsForEntityType returns the allowed uuid options for a given uuid entity
// type. A nil error with an empty slice means "no options available" (the
// interactive flow falls back to a free-text prompt in that case, and validation
// accepts any string).
//
// agentUUID scopes LOCAL_AGENT_CONNECTION options to a single agent; passing ""
// lists the connections of every online agent, which is what `describe` and
// non-interactive validation want.
func (r *ConfigResolver) OptionsForEntityType(declared extensiongen.EntityType, fieldIdentifier string, agentUUID string) ([]ConfigOption, error) {
	switch EffectiveEntityType(declared, fieldIdentifier) {
	case extensiongen.EntityType_ENTITY_TYPE_ENTITY_STANDALONE:
		if !r.entitiesDone {
			entities, err := r.er.GetStandaloneEntities(r.projectVersionUUID)
			if err != nil {
				return nil, err
			}
			for _, e := range entities {
				r.entities = append(r.entities, ConfigOption{Value: e.Uuid, Label: e.Identifier})
			}
			r.entitiesDone = true
		}
		return r.entities, nil
	case extensiongen.EntityType_ENTITY_TYPE_DB_CONNECTION:
		if !r.connDone {
			connections, err := r.er.GetTeamConnections(r.project.TeamUuid)
			if err != nil {
				return nil, err
			}
			for _, c := range connections {
				r.connections = append(r.connections, ConfigOption{Value: c.Uuid, Label: c.Identifier})
			}
			r.connDone = true
		}
		return r.connections, nil
	case extensiongen.EntityType_ENTITY_TYPE_DB_STORE:
		if !r.storesDone {
			stores, err := r.er.GetTeamObjectStores(r.project.TeamUuid)
			if err != nil {
				return nil, err
			}
			for _, s := range stores {
				r.stores = append(r.stores, ConfigOption{Value: s.Uuid, Label: s.Identifier})
			}
			r.storesDone = true
		}
		return r.stores, nil
	case extensiongen.EntityType_ENTITY_TYPE_LOCAL_AGENT:
		agents, err := r.onlineAgents()
		if err != nil {
			return nil, err
		}
		opts := make([]ConfigOption, 0, len(agents))
		for _, a := range agents {
			opts = append(opts, ConfigOption{Value: a.GetUuid(), Label: a.GetMachineName()})
		}
		return opts, nil
	case extensiongen.EntityType_ENTITY_TYPE_LOCAL_AGENT_CONNECTION:
		agents, err := r.onlineAgents()
		if err != nil {
			return nil, err
		}
		var opts []ConfigOption
		for _, a := range agents {
			if agentUUID != "" && a.GetUuid() != agentUUID {
				continue
			}
			for _, c := range a.GetConnections() {
				opts = append(opts, ConfigOption{
					Value: c.GetUuid(),
					Label: a.GetMachineName() + "/" + c.GetName(),
				})
			}
		}
		return opts, nil
	default:
		return nil, nil
	}
}

// onlineAgents fetches the caller's online agents once per resolver.
func (r *ConfigResolver) onlineAgents() ([]*nemgen.LocalAgent, error) {
	if !r.agentsDone {
		agents, err := r.er.GetOnlineLocalAgents()
		if err != nil {
			return nil, err
		}
		r.agents = agents
		r.agentsDone = true
	}
	return r.agents, nil
}

// enumOptions converts a field's enum options into ConfigOptions. The config
// value stored/expected by extensions is the option Identifier.
func enumOptions(cfg *extensiongen.ExtensionInputTypeEnumConfig) []ConfigOption {
	if cfg == nil {
		return nil
	}
	opts := make([]ConfigOption, 0, len(cfg.Options))
	for _, o := range cfg.Options {
		opts = append(opts, ConfigOption{Value: o.Identifier, Label: o.Value})
	}
	return opts
}

// fieldSchema builds the schema for a single configuration field, resolving
// option lists for uuid/enum (including array-of-uuid and array-of-enum).
func (r *ConfigResolver) fieldSchema(field *extensiongen.ExtensionInputField) (ConfigFieldSchema, error) {
	fs := ConfigFieldSchema{
		Identifier:  field.Identifier,
		DisplayName: field.DisplayName,
		Description: field.Description,
		Required:    field.Required,
		Type:        typeName(field.Type),
	}

	switch field.Type {
	case extensiongen.ExtensionInputType_EXTENSION_INPUT_TYPE_UUID:
		opts, err := r.OptionsForEntityType(UUIDFieldEntityType(field), field.Identifier, "")
		if err != nil {
			return fs, err
		}
		fs.Options = opts

	case extensiongen.ExtensionInputType_EXTENSION_INPUT_TYPE_ENUM:
		if field.TypeConfig != nil && field.TypeConfig.Enum != nil {
			fs.Multiple = field.TypeConfig.Enum.AllowMultiple
			fs.Options = enumOptions(field.TypeConfig.Enum)
		}

	case extensiongen.ExtensionInputType_EXTENSION_INPUT_TYPE_ARRAY:
		fs.Multiple = true
		if field.TypeConfig != nil && field.TypeConfig.Array != nil {
			arr := field.TypeConfig.Array
			fs.Type = typeName(arr.ArrayType)
			switch arr.ArrayType {
			case extensiongen.ExtensionInputType_EXTENSION_INPUT_TYPE_UUID:
				if arr.ArrayTypeConfig != nil && arr.ArrayTypeConfig.Uuid != nil {
					opts, err := r.OptionsForEntityType(arr.ArrayTypeConfig.Uuid.EntityType, field.Identifier, "")
					if err != nil {
						return fs, err
					}
					fs.Options = opts
				}
			case extensiongen.ExtensionInputType_EXTENSION_INPUT_TYPE_ENUM:
				if arr.ArrayTypeConfig != nil && arr.ArrayTypeConfig.Enum != nil {
					fs.Options = enumOptions(arr.ArrayTypeConfig.Enum)
				}
			}
		}
	}

	return fs, nil
}

// DescribeConfig produces the full, machine-readable configuration schema for an
// extension against a specific project version.
func (i *Implementation) DescribeConfig(
	project *nemgen.Project,
	projectVersion *nemgen.ProjectVersion,
	extension *nemgen.Extension,
	extensionVersion *nemgen.ExtensionVersion,
	configEntity *extensiongen.ExtensionConfigurationEntity,
	lastConfig map[string]interface{},
) (*ConfigSchema, error) {
	resolver := i.NewConfigResolver(project, projectVersion.Uuid)

	schema := &ConfigSchema{
		Extension: SchemaExtension{
			Identifier:  extension.Identifier,
			DisplayName: extension.DisplayName,
			Version:     extensionVersion.DisplayVersion,
			VersionUUID: extensionVersion.Uuid,
		},
		Project:        SchemaRef{Uuid: project.Uuid, Identifier: project.Name},
		ProjectVersion: SchemaRef{Uuid: projectVersion.Uuid, Identifier: projectVersion.Identifier},
		LastUsedConfig: lastConfig,
		Fields:         []ConfigFieldSchema{},
	}

	if configEntity != nil {
		for _, field := range configEntity.Fields {
			fs, err := resolver.fieldSchema(field)
			if err != nil {
				return nil, err
			}
			schema.Fields = append(schema.Fields, fs)
		}
	}

	return schema, nil
}
