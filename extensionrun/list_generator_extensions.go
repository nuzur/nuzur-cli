package extensionrun

import (
	nemgen "github.com/nuzur/nem/idl/gen"
	"github.com/nuzur/nuzur-cli/productclient"
	"github.com/nuzur/nuzur-cli/protodeps/gen"
)

func (i *Implementation) ListGeneratorExtensions() ([]*nemgen.Extension, error) {
	ctx, err := productclient.ClientContext()
	if err != nil {
		return nil, err
	}

	res, err := i.productClient.ProductClient.ListExtensions(ctx, &gen.ListExtensionsRequest{
		PageSize:      100,
		PageToken:     "",
		OrderBy:       "updated_at",
		PublishedOnly: true,
	})
	if err != nil {
		return nil, err
	}

	var generators []*nemgen.Extension
	for _, ext := range res.Extensions {
		if ext.GetExtensionType() == nemgen.ExtensionExtensionType_EXTENSION_EXTENSION_TYPE_GENERATOR {
			generators = append(generators, ext)
		}
	}

	return generators, nil
}
