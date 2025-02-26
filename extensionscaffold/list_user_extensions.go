package extensionscaffold

import (
	nemgen "github.com/nuzur/nem/idl/gen"
	"github.com/nuzur/nuzur-cli/productclient"
	"github.com/nuzur/nuzur-cli/protodeps/gen"
)

func (i *Implementation) ListUserExtensions() ([]*nemgen.Extension, error) {
	ctx, err := productclient.ClientContext()
	if err != nil {
		return nil, err
	}

	user, err := i.auth.GetTokenUser()
	if err != nil {
		return nil, err
	}

	res, err := i.productClient.ProductClient.ListExtensions(ctx, &gen.ListExtensionsRequest{
		OwnerUuid: user.Uuid,
		PageSize:  100,
		PageToken: "",
		OrderBy:   "updated_at",
	})
	if err != nil {
		return nil, err
	}

	return res.Extensions, nil
}
