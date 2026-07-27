package extensionrun

import (
	"fmt"

	nemgen "github.com/nuzur/nem/idl/gen"
	"github.com/nuzur/nuzur-cli/productclient"
	"github.com/nuzur/nuzur-cli/protodeps/gen"
)

// FindExtensionByIdentifier finds any published extension by identifier,
// regardless of type (generator, sql-push, etc.). Used by run-extension when an
// explicit --extension identifier is supplied so non-generator extensions are
// runnable too.
func (i *Implementation) FindExtensionByIdentifier(identifier string) (*nemgen.Extension, error) {
	ctx, err := productclient.ClientContext()
	if err != nil {
		return nil, err
	}
	res, err := i.productClient.ProductClient.ListExtensions(ctx, &gen.ListExtensionsRequest{
		PageSize:      100,
		OrderBy:       "updated_at",
		PublishedOnly: true,
	})
	if err != nil {
		return nil, err
	}
	for _, ext := range res.Extensions {
		if ext.Identifier == identifier {
			return ext, nil
		}
	}
	return nil, fmt.Errorf("the %q extension is not available for your account", identifier)
}

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
		if ext.GetExtensionType() == nemgen.ExtensionType_EXTENSION_TYPE_GENERATOR {
			generators = append(generators, ext)
		}
	}

	return generators, nil
}

// ListRunnableExtensions returns the extensions offered in the interactive
// picker: every published generator, plus the named pair fronts (importers and
// synchronizers like sql-push, which are otherwise not generators and so would
// never show up). The agent-side member of a pair is deliberately absent — it is
// reached by picking its front and answering the connection-mode question.
func (i *Implementation) ListRunnableExtensions(pairFronts []string) ([]*nemgen.Extension, error) {
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

	fronts := make(map[string]bool, len(pairFronts))
	for _, f := range pairFronts {
		fronts[f] = true
	}
	return filterRunnable(res.Extensions, fronts), nil
}

func filterRunnable(exts []*nemgen.Extension, fronts map[string]bool) []*nemgen.Extension {
	var runnable []*nemgen.Extension
	for _, ext := range exts {
		if ext.GetExtensionType() == nemgen.ExtensionType_EXTENSION_TYPE_GENERATOR || fronts[ext.GetIdentifier()] {
			runnable = append(runnable, ext)
		}
	}
	return runnable
}
