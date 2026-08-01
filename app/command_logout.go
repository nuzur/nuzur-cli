package app

import (
	"github.com/urfave/cli"
)

func (i *Implementation) LogoutCommand() cli.Command {
	return cli.Command{
		Name:  "logout",
		Usage: i.localize.Localize("logout_desc", "Logout"),
		Action: func(c *cli.Context) error {
			if err := requireNoArgs(c, "logout"); err != nil {
				return err
			}
			return i.auth.Logout(
				i.localize.Localize("logged_out", "Logged out"),
			)
		},
	}
}
