package auth

import (
	"sync"

	"github.com/nuzur/nuzur-cli/config"
	configprovider "go.uber.org/config"
)

type AuthClientImplementation struct {
	config   config.Config
	closeApp sync.WaitGroup
}

type Params struct {
	ConfigProvider configprovider.Provider
}

func New(params Params) (AuthClientImplementation, error) {
	config := config.Config{}
	params.ConfigProvider.Get("nuzur-cli").Populate(&config)
	return AuthClientImplementation{
		config: config,
	}, nil
}
