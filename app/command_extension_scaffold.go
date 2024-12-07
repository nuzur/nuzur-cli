package app

import (
	"github.com/nuzur/nuzur-cli/extensionscaffold"
	"github.com/urfave/cli"
)

func (i *Implementation) ExtensionScaffoldCommand() cli.Command {
	return cli.Command{
		Name:  "scaffold-extension",
		Usage: i.Localize("scaffold_extension_desc", "Scaffold the code for an extension"),
		Action: func(c *cli.Context) error {
			es, err := extensionscaffold.New(extensionscaffold.Params{
				Auth: i.auth,
			})

			if err != nil {
				return err
			}

			return es.Scaffold(extensionscaffold.ScaffoldParams{
				ExtensionUUID: "9c3c016f-d6b9-4a16-853e-1c84c369d659",
				Path:          ".",
			})
		},
	}
}
