package extensionscaffold

import (
	"bytes"
	"context"
	"embed"
	"errors"
	"fmt"
	"os/exec"
	"path"
	"path/filepath"
	"strings"

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

	// get extension
	extension, err := i.productClient.ProductClient.GetExtension(ctx, &gen.GetExtensionRequest{
		ExtensionUuid: params.ExtensionUUID,
	})
	if err != nil {
		return err
	}

	// get user
	user, err := i.auth.GetTokenUser()
	if err != nil {
		return err
	}

	if extension.CreatedByUuid != user.Uuid {
		return errors.New("not the owner of the extension")
	}

	// get extension version
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

	// generate files

	// top level
	err = genFile(ctx, params.Path, "main.go", genData)
	if err != nil {
		return err
	}
	err = genFile(ctx, params.Path, "readme.md", genData)
	if err != nil {
		return err
	}
	err = genFile(ctx, params.Path, "Dockerfile", genData)
	if err != nil {
		return err
	}

	// constants
	err = genFile(ctx, params.Path, "constants/constants.go", genData)
	if err != nil {
		return err
	}

	// config
	err = genFile(ctx, params.Path, "config/configvalues.go", genData)
	if err != nil {
		return err
	}
	err = genFile(ctx, params.Path, "config/extension.yaml", genData)
	if err != nil {
		return err
	}

	// server
	err = genFile(ctx, params.Path, "server/get_execution.go", genData)
	if err != nil {
		return err
	}
	err = genFile(ctx, params.Path, "server/metadata.go", genData)
	if err != nil {
		return err
	}
	err = genFile(ctx, params.Path, "server/server.go", genData)
	if err != nil {
		return err
	}
	err = genFile(ctx, params.Path, "server/start_execution.go", genData)
	if err != nil {
		return err
	}
	err = genFile(ctx, params.Path, "server/submit_step.go", genData)
	if err != nil {
		return err
	}

	// go mod init
	var out bytes.Buffer
	var stderr bytes.Buffer
	if !filetools.FileExists(path.Join(params.Path, "go.mod")) {
		cmd := exec.Command("go", "mod", "init", params.Module)
		cmd.Dir = params.Path
		cmd.Stdout = &out
		cmd.Stderr = &stderr
		err := cmd.Run()
		if err != nil {
			fmt.Printf("error running go mod init: %v | %v | %v\n", err, out.String(), stderr.String())
		}
	} else {
		fmt.Printf("go.mod already exists\n")
	}

	// go mod tidy
	cmd := exec.Command("go", "mod", "tidy")
	cmd.Dir = params.Path
	cmd.Stdout = &out
	cmd.Stderr = &stderr
	err = cmd.Run()
	if err != nil {
		fmt.Printf("error running go mod init: %v | %v | %v\n", err, out.String(), stderr.String())
	}

	return nil
}

func genFile(ctx context.Context, path string, fileName string, genData GenData) error {
	isGoFile := strings.Contains(fileName, ".go")
	tmplBytes, err := templates.ReadFile(fmt.Sprintf("templates/%s.tmpl", fileName))
	if err != nil {
		return err
	}
	_, err = filetools.GenerateFile(ctx, filetools.FileRequest{
		OutputPath:      filepath.Join(path, fileName),
		TemplateBytes:   tmplBytes,
		Data:            genData,
		DisableGoFormat: !isGoFile,
	})
	if err != nil {
		return err
	}
	return nil
}
