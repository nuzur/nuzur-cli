package extensionscaffold

import (
	"github.com/nuzur/nuzur-cli/productclient"
)

type Implementation struct {
	productClient *productclient.Client
}

type Params struct {
}

func New() (*Implementation, error) {
	pc, err := productclient.New(productclient.Params{})
	if err != nil {
		return nil, err
	}

	return &Implementation{
		productClient: pc,
	}, nil
}

type ScaffoldParams struct {
	ExtensionUUID string
	Path          string
}

func (i *Implementation) Scaffold(params ScaffoldParams) {

}
