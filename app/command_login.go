package app

import (
	"github.com/nuzur/nuzur-cli/auth"
	"github.com/urfave/cli"
)

func (i *Implementation) LoginCommand() cli.Command {
	return cli.Command{
		Name:  "login",
		Usage: i.Localize("login_desc", "Will redirect to the browser if needed to login into nuzur"),
		Action: func(c *cli.Context) error {
			return i.Login()
		},
	}
}

func (i *Implementation) Login() error {
	return i.auth.Login(auth.LoginParams{
		LoggedIn:  i.Localize("logged_in", "Logged in as"),
		LoggedOut: i.Localize("logged_out", "Logged out"),
	})
}
