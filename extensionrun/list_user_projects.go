package extensionrun

import (
	nemgen "github.com/nuzur/nem/idl/gen"
	"github.com/nuzur/nuzur-cli/productclient"
	"github.com/nuzur/nuzur-cli/protodeps/gen"
)

func (i *Implementation) ListUserProjects() ([]*nemgen.Project, error) {
	ctx, err := productclient.ClientContext()
	if err != nil {
		return nil, err
	}

	res, err := i.productClient.ProductClient.ListProjectsForUser(ctx, &gen.ListProjectsForUserRequest{
		PageSize: 100,
		OrderBy:  "updated_at",
	})
	if err != nil {
		return nil, err
	}

	return res.Projects, nil
}
