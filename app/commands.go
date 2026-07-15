package app

import "github.com/urfave/cli"

func (i *Implementation) Commands() []cli.Command {
	return []cli.Command{
		i.LoginCommand(),
		i.LogoutCommand(),
		i.ExtensionScaffoldCommand(),
		i.ExtensionUpdateConfigCommand(),
		i.ExtensionRunCommand(),
		i.GoCodeGenCommand(),
		i.ExtensionCheckLimitCommand(),
		i.AgentCommand(),
		i.DeployCommand(),
		i.DestroyCommand(),
	}
}
