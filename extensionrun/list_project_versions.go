package extensionrun

import (
	nemgen "github.com/nuzur/nem/idl/gen"
	"github.com/nuzur/nuzur-cli/productclient"
	"github.com/nuzur/nuzur-cli/protodeps/gen"
)

func (i *Implementation) ListProjectVersions(projectUUID string) ([]*nemgen.ProjectVersion, error) {
	ctx, err := productclient.ClientContext()
	if err != nil {
		return nil, err
	}

	res, err := i.productClient.ProductClient.ListProjectVersionsForUser(ctx, &gen.ListProjectVersionsForUserRequest{
		PageSize:          100,
		ProjectUuid:       projectUUID,
		ExcludeJsonFields: true,
	})
	if err != nil {
		return nil, err
	}

	return res.ProjectVersions, nil
}
