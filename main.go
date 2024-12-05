package main

import (
	"log"
	"os"
	"sort"

	"github.com/nuzur/nuzur-cli/auth"
	"github.com/nuzur/nuzur-cli/config"
	"github.com/urfave/cli"
)

func main() {
	configProvider, err := config.New()
	if err != nil {
		log.Fatalf("error getting config provider: %v\n", err)
	}

	auth, err := auth.New(auth.Params{
		ConfigProvider: configProvider,
	})
	if err != nil {
		log.Fatalf("error creating auth client: %v\n", err)
	}

	cliapp := cli.NewApp()
	cliapp.Name = "Nuzur CLI"
	cliapp.Usage = "Manage your nuzur projects and extensions"
	cliapp.Version = "0.0.1"
	cliapp.Author = "nuzur"
	cliapp.Description = "Nuzur CLI tools for developers to manage projects and extensions"

	cliapp.Commands = []cli.Command{
		{
			Name:  "login",
			Usage: "Will redirect to the browser if needed to login into nuzur",
			Action: func(c *cli.Context) error {
				return auth.Login()
			},
		},
	}

	sort.Sort(cli.FlagsByName(cliapp.Flags))
	sort.Sort(cli.CommandsByName(cliapp.Commands))

	err = cliapp.Run(os.Args)
	if err != nil {
		log.Fatal(err)
	}

}
