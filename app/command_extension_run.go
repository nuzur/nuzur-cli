package app

import (
	"fmt"

	"github.com/manifoldco/promptui"
	extensiongen "github.com/nuzur/extension-sdk/idl/gen"
	nemgen "github.com/nuzur/nem/idl/gen"
	"github.com/nuzur/nuzur-cli/extensionrun"
	"github.com/nuzur/nuzur-cli/outputtools"
	"github.com/urfave/cli"
)

func (i *Implementation) ExtensionRunCommand() cli.Command {
	return cli.Command{
		Name:  "run-extension",
		Usage: i.localize.Localize("extension_run_desc", "Run an extension"),
		Action: func(c *cli.Context) error {
			err := i.Login()
			if err != nil {
				return err
			}

			er, err := extensionrun.New(extensionrun.Params{
				Auth: i.auth,
			})
			if err != nil {
				return err
			}

			// select project
			project, err := i.SelectProject(er)
			if err != nil {
				return err
			}

			// check user role
			role, err := er.GetUserRoleForProject(project.Uuid)
			if err != nil {
				return err
			}
			if role != nemgen.UserProjectRole_USER_PROJECT_ROLE_ADMIN &&
				role != nemgen.UserProjectRole_USER_PROJECT_ROLE_DEVELOPER {
				outputtools.PrintlnColored(
					i.localize.Localize("extension_run_no_access", "You do not have access to run extensions for this project."),
					outputtools.Red,
				)
				return nil
			}

			// select project version
			projectVersion, err := i.SelectProjectVersion(er, project.Uuid)
			if err != nil {
				return err
			}

			// select extension
			extension, err := i.SelectPublicExtension(er)
			if err != nil {
				return err
			}

			// check pro access if the extension is pro
			if extension.Pro {
				isProActive, err := er.IsProActiveForProject(project.Uuid)
				if err != nil {
					return err
				}
				if !isProActive {
					outputtools.PrintlnColored(
						i.localize.Localize("extension_run_no_pro", "This is a Pro extension and your project does not have an active Pro subscription."),
						outputtools.Red,
					)
					return nil
				}
			}

			// get latest extension version
			extensionVersion, err := er.GetLatestExtensionVersion(extension.Uuid)
			if err != nil {
				return err
			}

			fmt.Printf("%s: %s (version %s)\n", i.localize.Localize("extension_run_selected", "Selected extension"), extension.DisplayName, extensionVersion.DisplayVersion)

			// load config entity
			configEntity, err := er.GetConfigEntity(extensionVersion)
			if err != nil {
				return err
			}

			// fetch last used config for this project version
			allLastConfigs, err := er.GetLastUsedConfig(projectVersion.Uuid)
			if err != nil {
				allLastConfigs = nil
			}

			var lastConfig map[string]interface{}
			if allLastConfigs != nil {
				lastConfig = allLastConfigs[extension.Identifier]
			}

			// build config values
			configValues, err := i.BuildConfigValues(er, project, projectVersion.Uuid, configEntity, lastConfig)
			if err != nil {
				return err
			}

			// save config for next time
			if allLastConfigs == nil {
				allLastConfigs = make(map[string]map[string]interface{})
			}
			allLastConfigs[extension.Identifier] = configValues
			if saveErr := er.SaveLastUsedConfig(projectVersion.Uuid, allLastConfigs); saveErr != nil {
				// non-fatal: just log and continue
				fmt.Printf("warning: could not save config: %v\n", saveErr)
			}

			// ask for output path
			pathPrompt := promptui.Prompt{
				Label:   i.localize.Localize("extension_run_path", "Output path"),
				Default: ".",
			}
			outputPath, err := pathPrompt.Run()
			if err != nil {
				return err
			}

			// run the extension
			outputtools.PrintlnColored(
				i.localize.Localize("extension_run_running", "Running extension..."),
				outputtools.Blue,
			)

			err = er.Run(extensionrun.RunParams{
				Extension:          extension,
				ExtensionVersion:   extensionVersion,
				ProjectUUID:        project.Uuid,
				ProjectVersionUUID: projectVersion.Uuid,
				ConfigValues:       configValues,
				OutputPath:         outputPath,
			})
			if err != nil {
				outputtools.PrintlnColored(
					fmt.Sprintf("%s: %v", i.localize.Localize("extension_run_error", "Extension run failed"), err),
					outputtools.Red,
				)
				return nil
			}

			outputtools.PrintlnColored(
				i.localize.Localize("extension_run_success", "Extension ran successfully! Output written to: ")+outputPath,
				outputtools.Green,
			)
			return nil
		},
	}
}

func (i *Implementation) SelectProject(er *extensionrun.Implementation) (*nemgen.Project, error) {
	outputtools.PrintlnColored(i.localize.Localize("extension_run_loading_projects", "Loading projects..."), outputtools.Blue)
	projects, err := er.ListUserProjects()
	if err != nil {
		return nil, err
	}

	templates := &promptui.SelectTemplates{
		Label:    "{{ . }}?",
		Active:   "↠ {{ .Name | cyan }}",
		Inactive: "  {{ .Name | cyan }}",
		Selected: "↠ {{ .Name | red | cyan }}",
	}

	prompt := promptui.Select{
		Label:     i.localize.Localize("extension_run_select_project", "Select project"),
		Items:     projects,
		Templates: templates,
	}

	index, _, err := prompt.Run()
	if err != nil {
		return nil, err
	}
	return projects[index], nil
}

func (i *Implementation) SelectProjectVersion(er *extensionrun.Implementation, projectUUID string) (*nemgen.ProjectVersion, error) {
	outputtools.PrintlnColored(i.localize.Localize("extension_run_loading_versions", "Loading project versions..."), outputtools.Blue)
	versions, err := er.ListProjectVersions(projectUUID)
	if err != nil {
		return nil, err
	}

	templates := &promptui.SelectTemplates{
		Label:    "{{ . }}?",
		Active:   "↠ {{ .Identifier | cyan }}",
		Inactive: "  {{ .Identifier | cyan }}",
		Selected: "↠ {{ .Identifier | red | cyan }}",
	}

	prompt := promptui.Select{
		Label:     i.localize.Localize("extension_run_select_version", "Select project version"),
		Items:     versions,
		Templates: templates,
	}

	index, _, err := prompt.Run()
	if err != nil {
		return nil, err
	}
	return versions[index], nil
}

func (i *Implementation) BuildConfigValues(
	er *extensionrun.Implementation,
	project *nemgen.Project,
	projectVersionUUID string,
	configEntity *extensiongen.ExtensionConfigurationEntity,
	lastConfig map[string]interface{},
) (map[string]interface{}, error) {
	values := make(map[string]interface{})

	if configEntity == nil || len(configEntity.Fields) == 0 {
		return values, nil
	}

	// if we have a last config, ask user if they want to reuse it
	if len(lastConfig) > 0 {
		outputtools.PrintlnColored(
			i.localize.Localize("extension_run_last_config", "Previous configuration found:"),
			outputtools.Blue,
		)
		for _, field := range configEntity.Fields {
			if v, ok := lastConfig[field.Identifier]; ok {
				fmt.Printf("  %s: %s\n", field.DisplayName, configValueToDisplay(v))
			}
		}

		confirmPrompt := promptui.Select{
			Label: i.localize.Localize("extension_run_reuse_config", "Use previous configuration?"),
			Items: []string{
				i.localize.Localize("extension_run_reuse_yes", "Yes, use previous"),
				i.localize.Localize("extension_run_reuse_no", "No, enter new values"),
			},
		}
		idx, _, err := confirmPrompt.Run()
		if err != nil {
			return nil, err
		}
		if idx == 0 {
			return lastConfig, nil
		}
	}

	// prompt user for each field
	for _, field := range configEntity.Fields {
		label := field.DisplayName
		if field.Description != "" {
			label = fmt.Sprintf("%s (%s)", field.DisplayName, field.Description)
		}

		var lastVal interface{}
		if lastConfig != nil {
			lastVal = lastConfig[field.Identifier]
		}

		switch field.Type {
		case extensiongen.ExtensionInputType_EXTENSION_INPUT_TYPE_BOOLEAN:
			items := []string{"true", "false"}
			selectedIdx := 1
			if b, ok := lastVal.(bool); ok && b {
				selectedIdx = 0
			}
			boolPrompt := promptui.Select{
				Label:     label,
				Items:     items,
				CursorPos: selectedIdx,
			}
			_, choice, err := boolPrompt.Run()
			if err != nil {
				return nil, err
			}
			values[field.Identifier] = choice == "true"

		case extensiongen.ExtensionInputType_EXTENSION_INPUT_TYPE_UUID:
			if field.TypeConfig != nil && field.TypeConfig.Uuid != nil {
				switch field.TypeConfig.Uuid.EntityType {
				case extensiongen.EntityType_ENTITY_TYPE_ENTITY_STANDALONE:
					entities, err := er.GetStandaloneEntities(projectVersionUUID)
					if err != nil || len(entities) == 0 {
						// fall back to text prompt
						val, err := runTextPrompt(label, configValueToDisplay(lastVal), field.Required)
						if err != nil {
							return nil, err
						}
						values[field.Identifier] = val
					} else {
						identifiers := make([]string, len(entities))
						uuids := make([]string, len(entities))
						for idx, e := range entities {
							identifiers[idx] = e.Identifier
							uuids[idx] = e.Uuid
						}
						cursorPos := 0
						if lastStr, ok := lastVal.(string); ok {
							for idx, u := range uuids {
								if u == lastStr {
									cursorPos = idx
									break
								}
							}
						}
						entityPrompt := promptui.Select{
							Label:     label,
							Items:     identifiers,
							CursorPos: cursorPos,
						}
						selectedIdx, _, err := entityPrompt.Run()
						if err != nil {
							return nil, err
						}
						values[field.Identifier] = uuids[selectedIdx]
					}

				case extensiongen.EntityType_ENTITY_TYPE_DB_CONNECTION:
					connections, err := er.GetTeamConnections(project.TeamUuid)
					if err != nil || len(connections) == 0 {
						val, err := runTextPrompt(label, configValueToDisplay(lastVal), field.Required)
						if err != nil {
							return nil, err
						}
						values[field.Identifier] = val
					} else {
						identifiers := make([]string, len(connections))
						uuids := make([]string, len(connections))
						for idx, c := range connections {
							identifiers[idx] = c.Identifier
							uuids[idx] = c.Uuid
						}
						cursorPos := 0
						if lastStr, ok := lastVal.(string); ok {
							for idx, u := range uuids {
								if u == lastStr {
									cursorPos = idx
									break
								}
							}
						}
						connPrompt := promptui.Select{
							Label:     label,
							Items:     identifiers,
							CursorPos: cursorPos,
						}
						selectedIdx, _, err := connPrompt.Run()
						if err != nil {
							return nil, err
						}
						values[field.Identifier] = uuids[selectedIdx]
					}

				case extensiongen.EntityType_ENTITY_TYPE_DB_STORE:
					stores, err := er.GetTeamObjectStores(project.TeamUuid)
					if err != nil || len(stores) == 0 {
						val, err := runTextPrompt(label, configValueToDisplay(lastVal), field.Required)
						if err != nil {
							return nil, err
						}
						values[field.Identifier] = val
					} else {
						identifiers := make([]string, len(stores))
						uuids := make([]string, len(stores))
						for idx, s := range stores {
							identifiers[idx] = s.Identifier
							uuids[idx] = s.Uuid
						}
						cursorPos := 0
						if lastStr, ok := lastVal.(string); ok {
							for idx, u := range uuids {
								if u == lastStr {
									cursorPos = idx
									break
								}
							}
						}
						storePrompt := promptui.Select{
							Label:     label,
							Items:     identifiers,
							CursorPos: cursorPos,
						}
						selectedIdx, _, err := storePrompt.Run()
						if err != nil {
							return nil, err
						}
						values[field.Identifier] = uuids[selectedIdx]
					}

				default:
					val, err := runTextPrompt(label, configValueToDisplay(lastVal), field.Required)
					if err != nil {
						return nil, err
					}
					values[field.Identifier] = val
				}
			} else {
				val, err := runTextPrompt(label, configValueToDisplay(lastVal), field.Required)
				if err != nil {
					return nil, err
				}
				values[field.Identifier] = val
			}

		case extensiongen.ExtensionInputType_EXTENSION_INPUT_TYPE_ENUM:
			if field.TypeConfig != nil && field.TypeConfig.Enum != nil {
				options := make([]string, len(field.TypeConfig.Enum.Options))
				for idx, opt := range field.TypeConfig.Enum.Options {
					options[idx] = opt.Identifier
				}
				if field.TypeConfig.Enum.AllowMultiple {
					// multi-select: same add/remove loop as UUID arrays
					selectedOptions := []string{}
					if prev, ok := lastVal.([]interface{}); ok {
						for _, v := range prev {
							if s, ok := v.(string); ok {
								selectedOptions = append(selectedOptions, s)
							}
						}
					} else if prev, ok := lastVal.([]string); ok {
						selectedOptions = append(selectedOptions, prev...)
					}
					fmt.Printf("Select options for %s (add or remove, choose '(done)' when finished):\n", field.DisplayName)
					for {
						selectedSet := make(map[string]bool, len(selectedOptions))
						for _, o := range selectedOptions {
							selectedSet[o] = true
						}

						var menuItems []string
						var menuActions []func()
						menuItems = append(menuItems, "(done)")
						menuActions = append(menuActions, nil)

						for _, opt := range options {
							if !selectedSet[opt] {
								o := opt
								menuItems = append(menuItems, "+ "+o)
								menuActions = append(menuActions, func() {
									selectedOptions = append(selectedOptions, o)
								})
							}
						}
						for _, opt := range selectedOptions {
							o := opt
							menuItems = append(menuItems, "- "+o)
							menuActions = append(menuActions, func() {
								updated := make([]string, 0, len(selectedOptions)-1)
								for _, s := range selectedOptions {
									if s != o {
										updated = append(updated, s)
									}
								}
								selectedOptions = updated
							})
						}

						selectPrompt := promptui.Select{
							Label: fmt.Sprintf("%s [selected: %d]", label, len(selectedOptions)),
							Items: menuItems,
						}
						idx, _, err := selectPrompt.Run()
						if err != nil {
							return nil, err
						}
						if idx == 0 {
							break
						}
						menuActions[idx]()
					}
					values[field.Identifier] = selectedOptions
				} else {
					// single select
					cursorPos := 0
					if lastStr, ok := lastVal.(string); ok {
						for idx, opt := range options {
							if opt == lastStr {
								cursorPos = idx
								break
							}
						}
					}
					enumPrompt := promptui.Select{
						Label:     label,
						Items:     options,
						CursorPos: cursorPos,
					}
					_, choice, err := enumPrompt.Run()
					if err != nil {
						return nil, err
					}
					values[field.Identifier] = choice
				}
			} else {
				// no options configured — fall back to text
				val, err := runTextPrompt(label, configValueToDisplay(lastVal), field.Required)
				if err != nil {
					return nil, err
				}
				values[field.Identifier] = val
			}

		case extensiongen.ExtensionInputType_EXTENSION_INPUT_TYPE_ARRAY:
			// Check if the array element type is UUID — if so, offer select prompts
			if field.TypeConfig != nil && field.TypeConfig.Array != nil &&
				field.TypeConfig.Array.ArrayType == extensiongen.ExtensionInputType_EXTENSION_INPUT_TYPE_UUID &&
				field.TypeConfig.Array.ArrayTypeConfig != nil &&
				field.TypeConfig.Array.ArrayTypeConfig.Uuid != nil {

				uuidCfg := field.TypeConfig.Array.ArrayTypeConfig.Uuid
				var displayItems []string
				var uuidItems []string

				switch uuidCfg.EntityType {
				case extensiongen.EntityType_ENTITY_TYPE_ENTITY_STANDALONE:
					entities, err := er.GetStandaloneEntities(projectVersionUUID)
					if err == nil {
						for _, e := range entities {
							displayItems = append(displayItems, e.Identifier)
							uuidItems = append(uuidItems, e.Uuid)
						}
					}
				case extensiongen.EntityType_ENTITY_TYPE_DB_CONNECTION:
					connections, err := er.GetTeamConnections(project.TeamUuid)
					if err == nil {
						for _, c := range connections {
							displayItems = append(displayItems, c.Identifier)
							uuidItems = append(uuidItems, c.Uuid)
						}
					}
				case extensiongen.EntityType_ENTITY_TYPE_DB_STORE:
					stores, err := er.GetTeamObjectStores(project.TeamUuid)
					if err == nil {
						for _, s := range stores {
							displayItems = append(displayItems, s.Identifier)
							uuidItems = append(uuidItems, s.Uuid)
						}
					}
				}

				if len(displayItems) > 0 {
					// multi-select: keep selecting until user picks "(done)"
					selectedUUIDs := []string{}
					// pre-select previously chosen UUIDs
					if prev, ok := lastVal.([]interface{}); ok {
						for _, v := range prev {
							if s, ok := v.(string); ok {
								selectedUUIDs = append(selectedUUIDs, s)
							}
						}
					}
					fmt.Printf("Select items for %s (add or remove, choose '(done)' when finished):\n", field.DisplayName)
					for {
						// build lookup: uuid -> display name
						uuidToDisplay := make(map[string]string, len(uuidItems))
						for j, u := range uuidItems {
							uuidToDisplay[u] = displayItems[j]
						}
						selectedSet := make(map[string]bool, len(selectedUUIDs))
						for _, u := range selectedUUIDs {
							selectedSet[u] = true
						}

						// menu: (done), add items not yet selected, then remove items already selected
						var menuItems []string
						var menuActions []func()
						menuItems = append(menuItems, "(done)")
						menuActions = append(menuActions, nil)

						for j, u := range uuidItems {
							if !selectedSet[u] {
								display := displayItems[j]
								uuid := u
								menuItems = append(menuItems, "+ "+display)
								menuActions = append(menuActions, func() {
									selectedUUIDs = append(selectedUUIDs, uuid)
								})
							}
						}
						for _, u := range selectedUUIDs {
							uuid := u
							menuItems = append(menuItems, "- "+uuidToDisplay[uuid])
							menuActions = append(menuActions, func() {
								updated := make([]string, 0, len(selectedUUIDs)-1)
								for _, s := range selectedUUIDs {
									if s != uuid {
										updated = append(updated, s)
									}
								}
								selectedUUIDs = updated
							})
						}

						selectPrompt := promptui.Select{
							Label: fmt.Sprintf("%s [selected: %d]", label, len(selectedUUIDs)),
							Items: menuItems,
						}
						idx, _, err := selectPrompt.Run()
						if err != nil {
							return nil, err
						}
						if idx == 0 {
							break
						}
						menuActions[idx]()
					}
					values[field.Identifier] = selectedUUIDs
				} else {
					// no items available — fall back to text
					defaultStr := configValueToDisplay(lastVal)
					textPrompt := promptui.Prompt{
						Label:   label + " (comma-separated UUIDs)",
						Default: defaultStr,
					}
					val, err := textPrompt.Run()
					if err != nil {
						return nil, err
					}
					values[field.Identifier] = splitCSV(val)
				}
			} else {
				// non-UUID array — prompt for comma-separated values
				defaultStr := configValueToDisplay(lastVal)
				textPrompt := promptui.Prompt{
					Label:   label + " (comma-separated)",
					Default: defaultStr,
				}
				if field.Required {
					textPrompt.Validate = func(s string) error {
						if s == "" {
							return fmt.Errorf("this field is required")
						}
						return nil
					}
				}
				val, err := textPrompt.Run()
				if err != nil {
					return nil, err
				}
				values[field.Identifier] = splitCSV(val)
			}

		default:
			// STRING, UUID, INTEGER, FLOAT, DATE, DATETIME — text prompt
			val, err := runTextPrompt(label, configValueToDisplay(lastVal), field.Required)
			if err != nil {
				return nil, err
			}
			values[field.Identifier] = val
		}
	}

	return values, nil
}

// configValueToDisplay converts an interface{} config value to a human-readable string.
func configValueToDisplay(v interface{}) string {
	if v == nil {
		return ""
	}
	switch val := v.(type) {
	case string:
		return val
	case bool:
		if val {
			return "true"
		}
		return "false"
	case []interface{}:
		strs := make([]string, 0, len(val))
		for _, item := range val {
			strs = append(strs, fmt.Sprintf("%v", item))
		}
		return joinCSV(strs)
	case []string:
		return joinCSV(val)
	default:
		return fmt.Sprintf("%v", v)
	}
}

func runTextPrompt(label, defaultVal string, required bool) (string, error) {
	p := promptui.Prompt{
		Label:   label,
		Default: defaultVal,
	}
	if required {
		p.Validate = func(s string) error {
			if s == "" {
				return fmt.Errorf("this field is required")
			}
			return nil
		}
	}
	return p.Run()
}

func splitCSV(s string) []string {
	var result []string
	for _, part := range splitAndTrim(s, ",") {
		if part != "" {
			result = append(result, part)
		}
	}
	return result
}

func joinCSV(parts []string) string {
	result := ""
	for i, p := range parts {
		if i > 0 {
			result += ","
		}
		result += p
	}
	return result
}

func splitAndTrim(s, sep string) []string {
	var out []string
	start := 0
	for i := 0; i <= len(s)-len(sep); i++ {
		if s[i:i+len(sep)] == sep {
			out = append(out, trimSpace(s[start:i]))
			start = i + len(sep)
		}
	}
	out = append(out, trimSpace(s[start:]))
	return out
}

func trimSpace(s string) string {
	start, end := 0, len(s)
	for start < end && (s[start] == ' ' || s[start] == '\t') {
		start++
	}
	for end > start && (s[end-1] == ' ' || s[end-1] == '\t') {
		end--
	}
	return s[start:end]
}

func (i *Implementation) SelectPublicExtension(er *extensionrun.Implementation) (*nemgen.Extension, error) {
	outputtools.PrintlnColored(i.localize.Localize("extension_scaffold_loading", ""), outputtools.Blue)
	extensions, err := er.ListGeneratorExtensions()
	if err != nil {
		return nil, err
	}

	templates := &promptui.SelectTemplates{
		Label:    "{{ . }}?",
		Active:   "↠ {{ .Identifier | cyan }} ({{ .DisplayName | red }})",
		Inactive: "  {{ .Identifier | cyan }} ({{ .DisplayName | red }})",
		Selected: "↠ {{ .Identifier | red | cyan }}",
	}

	prompt := promptui.Select{
		Label:     i.localize.Localize("extension_scaffold_select_extension", "Select extension"),
		Items:     extensions,
		Templates: templates,
	}

	index, _, err := prompt.Run()
	if err != nil {
		return nil, err
	}
	return extensions[index], nil
}
