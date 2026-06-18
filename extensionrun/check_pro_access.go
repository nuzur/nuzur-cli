package extensionrun

import (
	"github.com/nuzur/nuzur-cli/productclient"
	"github.com/nuzur/nuzur-cli/protodeps/gen"
)

func (i *Implementation) CheckExtensionExecutionLimit(projectUUID string, extensionUUID string) (*gen.CheckExtensionExecutionLimitResponse, error) {
	ctx, err := productclient.ClientContext()
	if err != nil {
		return nil, err
	}

	res, err := i.productClient.ProductClient.CheckExtensionExecutionLimit(ctx, &gen.CheckExtensionExecutionLimitRequest{
		ProjectUuid:   projectUUID,
		ExtensionUuid: extensionUUID,
	})
	if err != nil {
		return nil, err
	}

	return res, nil
}
