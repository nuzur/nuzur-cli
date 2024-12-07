package extensionscaffold

import (
	"errors"
	"fmt"

	"github.com/nuzur/nuzur-cli/auth"
	"github.com/nuzur/nuzur-cli/productclient"
	"github.com/nuzur/nuzur-cli/protodeps/gen"
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

type ScaffoldParams struct {
	ExtensionUUID        string
	ExtensionVersionUUID string
	Path                 string
}

func (i *Implementation) Scaffold(params ScaffoldParams) error {
	ctx, err := productclient.ClientContext()
	if err != nil {
		return err
	}
	extension, err := i.productClient.ProductClient.GetExtension(ctx, &gen.GetExtensionRequest{
		ExtensionUuid: params.ExtensionUUID,
	})
	if err != nil {
		return err
	}

	user, err := i.auth.GetTokenUser()
	if err != nil {
		return err
	}

	if extension.CreatedByUuid != user.Uuid {
		return errors.New("not the owner of the extension")
	}

	fmt.Printf("ext: %v \n", extension)

	return nil
}
