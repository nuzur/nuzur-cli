package app

import (
	"fmt"

	"github.com/manifoldco/promptui"
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

			extension, err := i.SelectPublicExtension(er)
			if err != nil {
				return err
			}

			fmt.Printf("selected extension: %v\n", extension.DisplayName)

			prompt := promptui.Prompt{
				Label:   i.localize.Localize("extension_run_path", ""),
				Default: ".",
			}
			path, err := prompt.Run()
			if err != nil {
				return err
			}

			fmt.Printf("path: %v\n", path)

			return nil
		},
	}
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
