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

// configResolver resolves and caches the concrete option lists (entities,
// connections, object stores) for a project version, so uuid/enum fields can be
// described and validated without re-fetching per field. It is shared by both
// `describe` (to list options) and the non-interactive config apply (to validate
// membership).
type configResolver struct {
	er                 *Implementation
	project            *nemgen.Project
	projectVersionUUID string

	entities     []ConfigOption
	entitiesDone bool
	connections  []ConfigOption
	connDone     bool
	stores       []ConfigOption
	storesDone   bool
}

func (i *Implementation) newConfigResolver(project *nemgen.Project, projectVersionUUID string) *configResolver {
	return &configResolver{
		er:                 i,
		project:            project,
		projectVersionUUID: projectVersionUUID,
	}
}

// optionsForEntityType returns the allowed uuid options for a given uuid entity
// type. A nil error with an empty slice means "no options available" (the
// interactive flow falls back to a free-text prompt in that case, and validation
// accepts any string).
func (r *configResolver) optionsForEntityType(et extensiongen.EntityType) ([]ConfigOption, error) {
	switch et {
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
	default:
		return nil, nil
	}
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
func (r *configResolver) fieldSchema(field *extensiongen.ExtensionInputField) (ConfigFieldSchema, error) {
	fs := ConfigFieldSchema{
		Identifier:  field.Identifier,
		DisplayName: field.DisplayName,
		Description: field.Description,
		Required:    field.Required,
		Type:        typeName(field.Type),
	}

	switch field.Type {
	case extensiongen.ExtensionInputType_EXTENSION_INPUT_TYPE_UUID:
		if field.TypeConfig != nil && field.TypeConfig.Uuid != nil {
			opts, err := r.optionsForEntityType(field.TypeConfig.Uuid.EntityType)
			if err != nil {
				return fs, err
			}
			fs.Options = opts
		}

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
					opts, err := r.optionsForEntityType(arr.ArrayTypeConfig.Uuid.EntityType)
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
	resolver := i.newConfigResolver(project, projectVersion.Uuid)

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
