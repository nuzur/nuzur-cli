package extensionrun

import (
	"fmt"
	"strings"

	nemgen "github.com/nuzur/nem/idl/gen"
)

// The schema the generated JWT server is built around. Nuzur models are
// authored in English or Spanish, so each accepts the identifiers of both.
// This mirrors go-code-gen's project package, which is the source of truth:
// the two live in separate modules and cannot share a package.
var (
	userEntityIdentifiers     = []string{"user", "usuario"}
	userEmailFieldIdentifiers = []string{"email", "correo", "correo_electronico"}
	userPasswordIdentifiers   = []string{
		"password", "pass", "pwd", "password_hash",
		"contrasena", "contraseña", "clave",
	}
)

// jwtAuthConfigValue is the config value that selects the JWT server. Any other
// value (keycloak, disabled, unset) carries no schema dependency.
const jwtAuthConfigValue = "jwt"

// ValidateJWTAuthRequirements checks the project schema against what the
// generated JWT server needs, before generation runs. Without it the run
// "succeeds" and produces a workspace that only fails later at go build, or on
// deploy, remotely, during the docker build.
//
// It returns a *ConfigValidationError on the "auth" field when generation would
// break, plus any non-blocking warnings. Both are nil/empty when auth is not
// set to jwt.
func (i *Implementation) ValidateJWTAuthRequirements(projectVersionUUID string, configValues map[string]interface{}) (*ConfigValidationError, []string, error) {
	auth, _ := configValues["auth"].(string)
	if auth != jwtAuthConfigValue {
		return nil, nil, nil
	}

	// The resolved project version does not carry entities (it is fetched with
	// ExcludeJsonFields), so pull the full schema here.
	entities, err := i.GetStandaloneEntities(projectVersionUUID)
	if err != nil {
		return nil, nil, fmt.Errorf("checking jwt auth requirements: %w", err)
	}

	missing, warnings := checkJWTAuthSchema(entities)
	if len(missing) == 0 {
		return nil, warnings, nil
	}
	return &ConfigValidationError{
		Fields: []FieldError{{
			Field:   "auth",
			Message: fmt.Sprintf("jwt auth requires %s", strings.Join(missing, "; ")),
		}},
	}, warnings, nil
}

// checkJWTAuthSchema holds the rule itself, over standalone entities only.
// GetStandaloneEntities already filters to standalone, which is also what the
// generated core accessor requires, so a non-standalone "user" reads here as a
// missing one.
func checkJWTAuthSchema(entities []*nemgen.Entity) (missing []string, warnings []string) {
	userEntity := firstEntityNamed(entities, userEntityIdentifiers)
	if userEntity == nil {
		return []string{fmt.Sprintf("a standalone entity identified as %s", quotedList(userEntityIdentifiers))}, nil
	}
	name := userEntity.Identifier

	emailField := firstFieldNamed(userEntity, userEmailFieldIdentifiers)
	if emailField == nil {
		missing = append(missing, fmt.Sprintf("an email field (%s) on the %q entity", quotedList(userEmailFieldIdentifiers), name))
	} else if !hasSingleFieldIndex(userEntity, emailField) {
		// Without this index the generated repo never emits the fetch-by-email
		// select the signin and validate handlers call.
		missing = append(missing, fmt.Sprintf(
			"an index or unique index on the %q entity covering only the %q field",
			name, emailField.Identifier))
	}

	if firstFieldNamed(userEntity, userPasswordIdentifiers) == nil {
		warnings = append(warnings, fmt.Sprintf(
			"the %q entity has no password field (%s): the generated app will build, but sign in will always fail",
			name, quotedList(userPasswordIdentifiers)))
	}

	return missing, warnings
}

func firstEntityNamed(entities []*nemgen.Entity, identifiers []string) *nemgen.Entity {
	for _, id := range identifiers {
		for _, e := range entities {
			if e.Identifier == id {
				return e
			}
		}
	}
	return nil
}

func firstFieldNamed(entity *nemgen.Entity, identifiers []string) *nemgen.Field {
	if entity == nil {
		return nil
	}
	for _, id := range identifiers {
		for _, f := range entity.Fields {
			if f.Identifier == id {
				return f
			}
		}
	}
	return nil
}

// hasSingleFieldIndex reports whether the entity carries a non primary index
// whose only usable member is the given field. Date and datetime members are
// ignored, matching the filtering the generator applies when naming selects.
func hasSingleFieldIndex(entity *nemgen.Entity, field *nemgen.Field) bool {
	if entity.TypeConfig == nil || entity.TypeConfig.Standalone == nil {
		return false
	}
	for _, index := range entity.TypeConfig.Standalone.Indexes {
		if index == nil {
			continue
		}
		if index.Type != nemgen.IndexType_INDEX_TYPE_INDEX && index.Type != nemgen.IndexType_INDEX_TYPE_UNIQUE {
			continue
		}
		usable := []string{}
		for _, indexField := range index.Fields {
			target := fieldByUUID(entity, indexField.FieldUuid)
			if target == nil {
				continue
			}
			if target.Type == nemgen.FieldType_FIELD_TYPE_DATE || target.Type == nemgen.FieldType_FIELD_TYPE_DATETIME {
				continue
			}
			usable = append(usable, target.Uuid)
		}
		if len(usable) == 1 && usable[0] == field.Uuid {
			return true
		}
	}
	return false
}

func fieldByUUID(entity *nemgen.Entity, uuid string) *nemgen.Field {
	for _, f := range entity.Fields {
		if f.Uuid == uuid {
			return f
		}
	}
	return nil
}

func quotedList(values []string) string {
	quoted := make([]string, 0, len(values))
	for _, v := range values {
		quoted = append(quoted, fmt.Sprintf("%q", v))
	}
	return strings.Join(quoted, " or ")
}
