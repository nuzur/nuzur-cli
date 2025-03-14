package app

import (
	"log"
	"os"
	"sort"

	"github.com/nuzur/nuzur-cli/auth"
	nuzurconfig "github.com/nuzur/nuzur-cli/config"
	"github.com/nuzur/nuzur-cli/localize"
	"github.com/nuzur/nuzur-cli/productclient"
	"github.com/urfave/cli"
	"go.uber.org/config"
)

type Implementation struct {
	cliapp         *cli.App
	localize       *localize.Implementation
	configProvider config.Provider
	auth           *auth.AuthClientImplementation
	productClient  *productclient.Client
}

func New() (*Implementation, error) {
	configProvider, err := nuzurconfig.New()
	if err != nil {
		log.Fatalf("error getting config provider: %v\n", err)
		return nil, err
	}

	loc := localize.New()

	auth, err := auth.New(auth.Params{
		ConfigProvider: configProvider,
		Localize:       loc,
	})
	if err != nil {
		log.Fatalf("error creating auth client: %v\n", err)
		return nil, err
	}

	pc, err := productclient.New(productclient.Params{})
	if err != nil {
		return nil, err
	}

	imp := Implementation{
		localize:       loc,
		configProvider: configProvider,
		auth:           &auth,
		productClient:  pc,
	}

	imp.cliapp = initCliApp(imp)

	return &imp, nil
}

func (i *Implementation) Run() error {
	return i.cliapp.Run(os.Args)
}

func initCliApp(imp Implementation) *cli.App {
	cliapp := cli.NewApp()
	cliapp.Name = "Nuzur CLI"
	cliapp.Usage = imp.localize.Localize("app_usage", "Manage your nuzur projects and extensions")
	cliapp.Version = "0.0.11"
	cliapp.Author = "nuzur"
	cliapp.Description = imp.localize.Localize("app_desc", "Nuzur CLI tools for developers to manage projects and extensions")

	cliapp.Commands = imp.Commands()

	sort.Sort(cli.FlagsByName(cliapp.Flags))
	sort.Sort(cli.CommandsByName(cliapp.Commands))
	return cliapp
}
