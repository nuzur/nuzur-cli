package extensionrun

import (
	"github.com/nuzur/nuzur-cli/productclient"
	"github.com/nuzur/nuzur-cli/protodeps/gen"
)

func (i *Implementation) IsProActiveForProject(projectUUID string) (bool, error) {
	ctx, err := productclient.ClientContext()
	if err != nil {
		return false, err
	}

	res, err := i.productClient.ProductClient.IsProActiveForProject(ctx, &gen.IsProActiveForProjectRequest{
		ProjectUuid: projectUUID,
	})
	if err != nil {
		return false, err
	}

	return res.IsProActive, nil
}
