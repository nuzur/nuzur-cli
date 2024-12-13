package extensionscaffold

import (
	"embed"
	"errors"
	"path/filepath"

	"github.com/nuzur/filetools"
	"github.com/nuzur/nuzur-cli/auth"
	"github.com/nuzur/nuzur-cli/productclient"
	"github.com/nuzur/nuzur-cli/protodeps/gen"
	nemgen "github.com/nuzur/nuzur-cli/protodeps/nem/idl/gen"
)

//go:embed templates/**
var templates embed.FS

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
	Module               string
}

type GenData struct {
	ModulePath       string
	Extension        *nemgen.Extension
	ExtensionVersion *nemgen.ExtensionVersion
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

	extensionVersion, err := i.productClient.ProductClient.GetExtensionVersion(ctx, &gen.GetExtensionVersionRequest{
		VersionUuid: params.ExtensionVersionUUID,
	})
	if err != nil {
		return err
	}

	genData := GenData{
		ModulePath:       params.Module,
		Extension:        extension,
		ExtensionVersion: extensionVersion,
	}

	tmplBytes, err := templates.ReadFile("templates/main.go.tmpl")
	if err != nil {
		return err
	}
	_, err = filetools.GenerateFile(ctx, filetools.FileRequest{
		OutputPath:    filepath.Join(params.Path, "main.go"),
		TemplateBytes: tmplBytes,
		Data:          genData,
	})
	if err != nil {
		return err
	}

	return nil
}
