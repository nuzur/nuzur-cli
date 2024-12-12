package app

import (
	"github.com/urfave/cli"
)

func (i *Implementation) LogoutCommand() cli.Command {
	return cli.Command{
		Name:  "logout",
		Usage: i.localize.Localize("logout_desc", "Logout"),
		Action: func(c *cli.Context) error {
			return i.auth.Logout(
				i.localize.Localize("logged_out", "Logged out"),
			)
		},
	}
}
