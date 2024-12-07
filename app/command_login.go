package app

import (
	"github.com/nuzur/nuzur-cli/outputtools"
	"github.com/urfave/cli"
)

func (i *Implementation) LoginCommand() cli.Command {
	return cli.Command{
		Name:  "login",
		Usage: i.Localize("login_desc", "Will redirect to the browser if needed to login into nuzur"),
		Action: func(c *cli.Context) error {
			outputtools.PrintlnColored("Login", outputtools.Blue)
			return i.auth.Login()
		},
	}
}
