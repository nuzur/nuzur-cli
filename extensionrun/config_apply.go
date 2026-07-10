package extensionrun

import (
	"fmt"
	"strconv"
	"strings"

	extensiongen "github.com/nuzur/extension-sdk/idl/gen"
	nemgen "github.com/nuzur/nem/idl/gen"
)

// FieldError describes why a single config field failed validation. It is part
// of the stable JSON error contract emitted in --json mode.
type FieldError struct {
	Field   string `json:"field"`
	Message string `json:"message"`
}

// ConfigValidationError aggregates all per-field problems found while building a
// config non-interactively, so an agent gets every issue in one shot rather than
// one-at-a-time.
type ConfigValidationError struct {
	Fields []FieldError `json:"errors"`
}

func (e *ConfigValidationError) Error() string {
	parts := make([]string, 0, len(e.Fields))
	for _, f := range e.Fields {
		parts = append(parts, fmt.Sprintf("%s: %s", f.Field, f.Message))
	}
	return "invalid config: " + strings.Join(parts, "; ")
}

// BuildConfigFromJSON produces the final config value map for a non-interactive
// run. `provided` (parsed from --config JSON) is merged over `lastConfig`, then
// every field is coerced and validated against the schema. On any problem it
// returns a *ConfigValidationError listing all offending fields.
func (i *Implementation) BuildConfigFromJSON(
	project *nemgen.Project,
	projectVersionUUID string,
	configEntity *extensiongen.ExtensionConfigurationEntity,
	provided map[string]interface{},
	lastConfig map[string]interface{},
) (map[string]interface{}, error) {
	values := make(map[string]interface{})
	if configEntity == nil || len(configEntity.Fields) == 0 {
		return values, nil
	}

	resolver := i.newConfigResolver(project, projectVersionUUID)
	var fieldErrs []FieldError

	// known identifiers, to flag unknown keys the caller supplied (likely typos)
	known := make(map[string]bool, len(configEntity.Fields))
	for _, field := range configEntity.Fields {
		known[field.Identifier] = true
	}
	for key := range provided {
		if !known[key] {
			fieldErrs = append(fieldErrs, FieldError{
				Field:   key,
				Message: "unknown config field for this extension",
			})
		}
	}

	for _, field := range configEntity.Fields {
		raw, hasProvided := provided[field.Identifier]
		if !hasProvided {
			// fall back to the last-used value when the caller didn't supply one
			if lv, ok := lastConfig[field.Identifier]; ok {
				raw = lv
				hasProvided = true
			}
		}

		if !hasProvided || raw == nil {
			if field.Required {
				fieldErrs = append(fieldErrs, FieldError{
					Field:   field.Identifier,
					Message: "required field is missing",
				})
			}
			continue
		}

		value, err := resolver.coerceField(field, raw)
		if err != nil {
			fieldErrs = append(fieldErrs, FieldError{Field: field.Identifier, Message: err.Error()})
			continue
		}
		values[field.Identifier] = value
	}

	if len(fieldErrs) > 0 {
		return nil, &ConfigValidationError{Fields: fieldErrs}
	}
	return values, nil
}

// coerceField validates and normalizes a single raw value into the shape the
// extension expects (mirroring the interactive BuildConfigValues: scalars are
// stored as strings except booleans, arrays as []string).
func (r *configResolver) coerceField(field *extensiongen.ExtensionInputField, raw interface{}) (interface{}, error) {
	switch field.Type {
	case extensiongen.ExtensionInputType_EXTENSION_INPUT_TYPE_BOOLEAN:
		return coerceBool(raw)

	case extensiongen.ExtensionInputType_EXTENSION_INPUT_TYPE_INTEGER,
		extensiongen.ExtensionInputType_EXTENSION_INPUT_TYPE_FLOAT:
		s, err := coerceScalarString(raw)
		if err != nil {
			return nil, err
		}
		if _, err := strconv.ParseFloat(s, 64); err != nil {
			return nil, fmt.Errorf("expected a number, got %q", s)
		}
		return s, nil

	case extensiongen.ExtensionInputType_EXTENSION_INPUT_TYPE_UUID:
		s, err := coerceScalarString(raw)
		if err != nil {
			return nil, err
		}
		if field.TypeConfig != nil && field.TypeConfig.Uuid != nil {
			opts, err := r.optionsForEntityType(field.TypeConfig.Uuid.EntityType)
			if err != nil {
				return nil, err
			}
			if err := validateOption(s, opts); err != nil {
				return nil, err
			}
		}
		return s, nil

	case extensiongen.ExtensionInputType_EXTENSION_INPUT_TYPE_ENUM:
		opts := enumOptions(fieldEnumConfig(field))
		if field.TypeConfig != nil && field.TypeConfig.Enum != nil && field.TypeConfig.Enum.AllowMultiple {
			return coerceStringArray(raw, opts)
		}
		s, err := coerceScalarString(raw)
		if err != nil {
			return nil, err
		}
		if err := validateOption(s, opts); err != nil {
			return nil, err
		}
		return s, nil

	case extensiongen.ExtensionInputType_EXTENSION_INPUT_TYPE_ARRAY:
		var opts []ConfigOption
		if field.TypeConfig != nil && field.TypeConfig.Array != nil {
			arr := field.TypeConfig.Array
			switch arr.ArrayType {
			case extensiongen.ExtensionInputType_EXTENSION_INPUT_TYPE_UUID:
				if arr.ArrayTypeConfig != nil && arr.ArrayTypeConfig.Uuid != nil {
					var err error
					opts, err = r.optionsForEntityType(arr.ArrayTypeConfig.Uuid.EntityType)
					if err != nil {
						return nil, err
					}
				}
			case extensiongen.ExtensionInputType_EXTENSION_INPUT_TYPE_ENUM:
				opts = enumOptions(arr.ArrayTypeConfig.GetEnum())
			}
		}
		return coerceStringArray(raw, opts)

	default:
		return coerceScalarString(raw)
	}
}

func fieldEnumConfig(field *extensiongen.ExtensionInputField) *extensiongen.ExtensionInputTypeEnumConfig {
	if field.TypeConfig == nil {
		return nil
	}
	return field.TypeConfig.Enum
}

// validateOption checks membership against a resolved option list. An empty list
// means options couldn't be enumerated (mirrors the interactive text-prompt
// fallback), so any value is accepted.
func validateOption(value string, opts []ConfigOption) error {
	if len(opts) == 0 {
		return nil
	}
	labels := make([]string, 0, len(opts))
	for _, o := range opts {
		if o.Value == value {
			return nil
		}
		labels = append(labels, o.Value)
	}
	return fmt.Errorf("value %q is not one of the allowed options: %s", value, strings.Join(labels, ", "))
}

func coerceBool(raw interface{}) (interface{}, error) {
	switch v := raw.(type) {
	case bool:
		return v, nil
	case string:
		b, err := strconv.ParseBool(v)
		if err != nil {
			return nil, fmt.Errorf("expected a boolean, got %q", v)
		}
		return b, nil
	default:
		return nil, fmt.Errorf("expected a boolean, got %T", raw)
	}
}

// coerceScalarString accepts strings, numbers and bools and normalizes to a
// string (JSON numbers arrive as float64).
func coerceScalarString(raw interface{}) (string, error) {
	switch v := raw.(type) {
	case string:
		return v, nil
	case bool:
		return strconv.FormatBool(v), nil
	case float64:
		return strconv.FormatFloat(v, 'f', -1, 64), nil
	case int:
		return strconv.Itoa(v), nil
	case int64:
		return strconv.FormatInt(v, 10), nil
	default:
		return "", fmt.Errorf("expected a scalar value, got %T", raw)
	}
}

// coerceStringArray normalizes an array value (JSON array, []string, or a
// comma-separated string) to []string and validates each element against opts.
func coerceStringArray(raw interface{}, opts []ConfigOption) (interface{}, error) {
	var out []string
	switch v := raw.(type) {
	case []interface{}:
		for _, item := range v {
			s, err := coerceScalarString(item)
			if err != nil {
				return nil, err
			}
			out = append(out, s)
		}
	case []string:
		out = append(out, v...)
	case string:
		for _, part := range strings.Split(v, ",") {
			part = strings.TrimSpace(part)
			if part != "" {
				out = append(out, part)
			}
		}
	default:
		return nil, fmt.Errorf("expected an array, got %T", raw)
	}

	for _, item := range out {
		if err := validateOption(item, opts); err != nil {
			return nil, err
		}
	}
	if out == nil {
		out = []string{}
	}
	return out, nil
}
