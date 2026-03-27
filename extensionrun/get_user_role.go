package extensionrun

import (
	nemgen "github.com/nuzur/nem/idl/gen"
	"github.com/nuzur/nuzur-cli/productclient"
	"github.com/nuzur/nuzur-cli/protodeps/gen"
)

func (i *Implementation) GetUserRoleForProject(projectUUID string) (nemgen.UserProjectRole, error) {
	ctx, err := productclient.ClientContext()
	if err != nil {
		return nemgen.UserProjectRole(0), err
	}

	res, err := i.productClient.ProductClient.GetTokenUserRoleForProject(ctx, &gen.GetUserRoleForProjectRequest{
		ProjectUuid: projectUUID,
	})
	if err != nil {
		return nemgen.UserProjectRole(0), err
	}

	return res.Role, nil
}
