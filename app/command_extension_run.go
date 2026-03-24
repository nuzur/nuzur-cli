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

			// select the project + project version to use first

			// check the user role for the project, if the user is not admin or developer
			// then show a message that the user does not have access to run the extension and return
			// if the user is admin or developer, then show the list of extensions of type generator that are public and ask the user to select one

			// then select the extension

			// if the user selects a pro extension, check if the user has pro access for the project
			// if the user does not have pro access, show a message that the user does not have access to run the extension and return

			// if the user has access:
			// try to fetch last used extension config and display it
			// ask the user to confirm if they want to use the same config or update it

			// if the user wants to update or the config does not exist,
			// then show the config options and ask the user to select/enter the values

			// then ask the user to enter the path to run the extension

			// then run the extension, fetch the output and place it in the specified path

			// if the extension run is successful, show a success message and return to the main menu
			// if the extension run fails, show an error message and return to the main menu

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
