package app

import "github.com/urfave/cli"

func (i *Implementation) Commands() []cli.Command {
	return []cli.Command{
		i.LoginCommand(),
	}
}