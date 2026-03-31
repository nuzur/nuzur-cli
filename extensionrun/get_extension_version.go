package extensionrun

import (
	"errors"

	nemgen "github.com/nuzur/nem/idl/gen"
	"github.com/nuzur/nuzur-cli/productclient"
	"github.com/nuzur/nuzur-cli/protodeps/gen"
)

func (i *Implementation) GetLatestExtensionVersion(extensionUUID string) (*nemgen.ExtensionVersion, error) {
	ctx, err := productclient.ClientContext()
	if err != nil {
		return nil, err
	}

	res, err := i.productClient.ProductClient.ListExtensionVersions(ctx, &gen.ListExtensionVersionsRequest{
		ExtensionUuid: extensionUUID,
		PageSize:      100,
		OrderBy:       "updated_at desc",
	})
	if err != nil {
		return nil, err
	}

	if len(res.Versions) == 0 {
		return nil, errors.New("no versions found for extension")
	}

	return res.Versions[0], nil
}
