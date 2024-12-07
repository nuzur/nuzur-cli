package app

import (
	"github.com/urfave/cli"
)

func (i *Implementation) LoginCommand() cli.Command {
	return cli.Command{
		Name:  "login",
		Usage: i.Localize("login_desc", "Will redirect to the browser if needed to login into nuzur"),
		Action: func(c *cli.Context) error {
			return i.auth.Login(i.Localize("login_success", "Logged in as"),
				i.Localize("login_error", "Could not login"),
			)
		},
	}
}
