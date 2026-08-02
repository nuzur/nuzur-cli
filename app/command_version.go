package app

import (
	"fmt"

	"github.com/nuzur/nuzur-cli/constants"
	"github.com/urfave/cli"
)

// VersionCommand mirrors the built-in --version flag as a subcommand, because
// `nuzur-cli version` is what people (and agents) type first.
func (i *Implementation) VersionCommand() cli.Command {
	return cli.Command{
		Name:  "version",
		Usage: i.localize.Localize("version_desc", "Print the CLI version"),
		Action: func(c *cli.Context) error {
			if err := requireNoArgs(c, "version"); err != nil {
				return err
			}
			fmt.Println("nuzur CLI version " + constants.CLI_VERSION)
			return nil
		},
	}
}
