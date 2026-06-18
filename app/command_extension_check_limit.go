package app

import (
	"fmt"

	"github.com/nuzur/nuzur-cli/extensionrun"
	"github.com/nuzur/nuzur-cli/outputtools"
	"github.com/urfave/cli"
)

func (i *Implementation) ExtensionCheckLimitCommand() cli.Command {
	return cli.Command{
		Name:  "extension-limit",
		Usage: i.localize.Localize("extension_check_limit_desc", "Check monthly execution limits for a Pro extension"),
		Action: func(c *cli.Context) error {
			err := i.Login()
			if err != nil {
				return err
			}

			er, err := extensionrun.New(extensionrun.Params{
				Auth: i.auth,
			})
			if err != nil {
				return err
			}

			// select project
			project, err := i.SelectProject(er)
			if err != nil {
				return err
			}

			// select extension
			extension, err := i.SelectPublicExtension(er)
			if err != nil {
				return err
			}

			outputtools.PrintlnColored(
				i.localize.Localize("extension_check_limit_loading", "Checking execution limit..."),
				outputtools.Blue,
			)

			// Check monthly execution limits
			limitRes, err := er.CheckExtensionExecutionLimit(project.Uuid, extension.Uuid)
			if err != nil {
				return err
			}

			if limitRes.Limit == 0 {
				outputtools.PrintlnColored(
					i.localize.Localize("extension_check_limit_unlimited", "You have unlimited executions for this extension."),
					outputtools.Green,
				)
				return nil
			}

			limitReachedStr := i.localize.Localize("no", "No")
			color := outputtools.Green
			if limitRes.IsLimited {
				limitReachedStr = i.localize.Localize("yes", "Yes")
				color = outputtools.Red
			}

			statusMsg := i.localize.LocalizeWithVariables(
				"extension_check_limit_status",
				map[string]string{
					"current":   fmt.Sprintf("%d", limitRes.Current),
					"limit":     fmt.Sprintf("%d", limitRes.Limit),
					"isLimited": limitReachedStr,
				},
				fmt.Sprintf("Monthly usage: %d / %d executions (Limit reached: %s)", limitRes.Current, limitRes.Limit, limitReachedStr),
			)

			outputtools.PrintlnColored(statusMsg, color)
			return nil
		},
	}
}
