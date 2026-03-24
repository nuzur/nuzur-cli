package extensionrun

import (
	"github.com/nuzur/nuzur-cli/auth"
	"github.com/nuzur/nuzur-cli/productclient"
)

type Implementation struct {
	productClient *productclient.Client
	auth          *auth.AuthClientImplementation
}

type Params struct {
	Auth *auth.AuthClientImplementation
}

func New(params Params) (*Implementation, error) {
	pc, err := productclient.New(productclient.Params{})
	if err != nil {
		return nil, err
	}

	return &Implementation{
		productClient: pc,
		auth:          params.Auth,
	}, nil
}
