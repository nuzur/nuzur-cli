package auth

import (
	"sync"

	"github.com/nuzur/nuzur-cli/config"
	"github.com/nuzur/nuzur-cli/localize"
	configprovider "go.uber.org/config"
)

type AuthClientImplementation struct {
	config   config.Config
	closeApp sync.WaitGroup
	localize *localize.Implementation
}

type Params struct {
	ConfigProvider configprovider.Provider
	Localize       *localize.Implementation
}

func New(params Params) (AuthClientImplementation, error) {
	config := config.Config{}
	params.ConfigProvider.Get("nuzur-cli").Populate(&config)
	return AuthClientImplementation{
		config:   config,
		localize: params.Localize,
	}, nil
}
