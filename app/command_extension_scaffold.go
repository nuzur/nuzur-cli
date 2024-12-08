package app

import (
	"github.com/manifoldco/promptui"
	"github.com/nuzur/nuzur-cli/extensionscaffold"
	"github.com/nuzur/nuzur-cli/outputtools"
	"github.com/nuzur/nuzur-cli/productclient"
	"github.com/nuzur/nuzur-cli/protodeps/gen"
	nemgen "github.com/nuzur/nuzur-cli/protodeps/nem/idl/gen"
	"github.com/urfave/cli"
)

func (i *Implementation) ExtensionScaffoldCommand() cli.Command {
	return cli.Command{
		Name:  "scaffold-extension",
		Usage: i.Localize("extension_scaffold_desc", "Scaffold the code for an extension"),
		Action: func(c *cli.Context) error {
			err := i.Login()
			if err != nil {
				return err
			}

			es, err := extensionscaffold.New(extensionscaffold.Params{
				Auth: i.auth,
			})
			if err != nil {
				return err
			}

			selectedExtension, err := i.SelectExtension(es)
			if err != nil {
				return err
			}

			selectedVersion, err := i.SelectExtensionVersion(selectedExtension.Uuid)
			if err != nil {
				return err
			}

			prompt := promptui.Prompt{
				Label:   i.Localize("extension_scaffold_path", ""),
				Default: ".",
			}
			path, err := prompt.Run()
			if err != nil {
				return err
			}

			return es.Scaffold(extensionscaffold.ScaffoldParams{
				ExtensionUUID:        selectedExtension.Uuid,
				ExtensionVersionUUID: selectedVersion.Uuid,
				Path:                 path,
			})
		},
	}
}

func (i *Implementation) SelectExtension(es *extensionscaffold.Implementation) (*nemgen.Extension, error) {
	outputtools.PrintlnColored(i.Localize("extension_scaffold_loading", ""), outputtools.Blue)
	extensions, err := es.ListUserExtensions()
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
		Label:     i.Localize("extension_scaffold_select_extension", "Select extension"),
		Items:     extensions,
		Templates: templates,
	}

	index, _, err := prompt.Run()
	if err != nil {
		return nil, err
	}
	return extensions[index], nil
}

func (i *Implementation) SelectExtensionVersion(extensionUUID string) (*nemgen.ExtensionVersion, error) {
	outputtools.PrintlnColored(i.Localize("extension_scaffold_loading_versions", ""), outputtools.Blue)
	ctx, err := productclient.ClientContext()
	if err != nil {
		return nil, err
	}
	res, err := i.productClient.ProductClient.ListExtensionVersions(ctx, &gen.ListExtensionVersionsRequest{
		ExtensionUuid: extensionUUID,
		PageSize:      100,
		PageToken:     "",
		OrderBy:       "updated_at",
	})
	if err != nil {
		return nil, err
	}

	versions := res.Versions

	templates := &promptui.SelectTemplates{
		Label:    "{{ . }}?",
		Active:   "↠ {{ .DisplayVersion | cyan }} ({{ .Description | red }})",
		Inactive: "  {{ .DisplayVersion | cyan }} ({{ .Description | red }})",
		Selected: "↠ {{ .DisplayVersion | red | cyan }}",
	}

	prompt := promptui.Select{
		Label:     i.Localize("extension_scaffold_select_extension_version", "Select extension version"),
		Items:     versions,
		Templates: templates,
	}

	index, _, err := prompt.Run()
	if err != nil {
		return nil, err
	}
	return versions[index], nil
}
