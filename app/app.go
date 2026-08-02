package app

import (
	"log"
	"net/http"
	"os"
	"sort"

	"github.com/nuzur/nuzur-cli/auth"
	nuzurconfig "github.com/nuzur/nuzur-cli/config"
	"github.com/nuzur/nuzur-cli/constants"
	"github.com/nuzur/nuzur-cli/deploy"
	"github.com/nuzur/nuzur-cli/extensionrun"
	"github.com/nuzur/nuzur-cli/localize"
	"github.com/nuzur/nuzur-cli/outputtools"
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

	// Seams. Every one of these is nil in production, and nil means "use the real
	// thing" — see the accessors below, which are the only readers. They exist so
	// the deploy pipeline can be driven end to end without an SSH host, a cloud
	// provider, the extension server or a browser login; nothing in the shipped
	// binary ever sets them.
	newSSHRunner       func(deploy.Target) deploy.RemoteRunner
	newProvisioner     func(deploy.Provider) (deploy.Provisioner, error)
	newExtensionRunner func() (extensionRunner, error)
	loginFn            func() error
	// httpTransport backs the CLI's own outbound HTTP — today only the
	// release-asset probe (see deploy_release_probe.go). nil is the real
	// transport, so nothing has to be wired up for the shipped binary.
	httpTransport http.RoundTripper
}

// sshRunner builds the runner the deploy/destroy paths talk to the box through.
//
// The production runner's live command output is pointed at the CLI's own stderr
// sink rather than left to default to os.Stderr, so the box's bootstrap output
// lands in the same place as everything else the CLI prints (outputtools.Stderr
// IS os.Stderr unless something swapped it).
func (i *Implementation) sshRunner(t deploy.Target) deploy.RemoteRunner {
	if i.newSSHRunner != nil {
		return i.newSSHRunner(t)
	}
	r := deploy.NewSSHRunner(t)
	r.Stderr = outputtools.Stderr
	return r
}

// provisioner resolves the provider adapter that creates/destroys the VM.
func (i *Implementation) provisioner(p deploy.Provider) (deploy.Provisioner, error) {
	if i.newProvisioner != nil {
		return i.newProvisioner(p)
	}
	return deploy.NewProvisioner(p)
}

// extensionRunner builds the client the CLI runs extensions through.
func (i *Implementation) extensionRunner() (extensionRunner, error) {
	if i.newExtensionRunner != nil {
		return i.newExtensionRunner()
	}
	return extensionrun.New(extensionrun.Params{Auth: i.auth})
}

// login is the seam-aware form of Login: the deploy, destroy and extension-run
// paths go through it so a test does not have to reach the real auth service
// (auth.Login dials nuzur for the token's user).
func (i *Implementation) login() error {
	if i.loginFn != nil {
		return i.loginFn()
	}
	return i.Login()
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
	cliapp.Version = constants.CLI_VERSION
	cliapp.Author = "nuzur"
	cliapp.Description = imp.localize.Localize("app_desc", "Nuzur CLI tools for developers to manage projects and extensions")

	cliapp.Commands = imp.Commands()

	sort.Sort(cli.FlagsByName(cliapp.Flags))
	sort.Sort(cli.CommandsByName(cliapp.Commands))
	return cliapp
}
