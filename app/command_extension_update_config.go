package app

import (
	"github.com/manifoldco/promptui"
	"github.com/nuzur/nuzur-cli/extensionscaffold"
	"github.com/urfave/cli"
)

func (i *Implementation) ExtensionUpdateConfigCommand() cli.Command {
	return cli.Command{
		Name:  "extension-update-config",
		Usage: i.localize.Localize("extension_update_config_desc", "Update the configuration of an extension"),
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
				Label:   i.localize.Localize("extension_scaffold_path", ""),
				Default: ".",
			}
			path, err := prompt.Run()
			if err != nil {
				return err
			}

			return es.ExtensionUpdateConfig(extensionscaffold.ExtensionUpdateConfigParams{
				ExtensionUUID:        selectedExtension.Uuid,
				ExtensionVersionUUID: selectedVersion.Uuid,
				Path:                 path,
			})
		},
	}
}
