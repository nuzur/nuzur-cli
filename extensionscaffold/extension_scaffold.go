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
	"text/template"

	gopluralize "github.com/gertd/go-pluralize"
	"github.com/iancoleman/strcase"
	extensiongen "github.com/nuzur/extension-sdk/idl/gen"
	"github.com/nuzur/filetools"
	nemgen "github.com/nuzur/nem/idl/gen"
	"github.com/nuzur/nuzur-cli/auth"
	"github.com/nuzur/nuzur-cli/productclient"
	"github.com/nuzur/nuzur-cli/protodeps/gen"
	"google.golang.org/protobuf/encoding/protojson"
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
	ConfigEntity     *extensiongen.ExtensionConfigurationEntity
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

	// unmarshall extension config entity from the version
	extensionConfigEntity := extensiongen.ExtensionConfigurationEntity{}
	err = protojson.Unmarshal([]byte(extensionVersion.ConfigurationEntity), &extensionConfigEntity)
	if err != nil {
		return err
	}

	genData := GenData{
		ModulePath:       params.Module,
		Extension:        extension,
		ExtensionVersion: extensionVersion,
		ConfigEntity:     &extensionConfigEntity,
	}

	// generate files

	// top level
	fmt.Printf("[extension-scaffold] generating top level files\n")
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
	fmt.Printf("[extension-scaffold] generating constants\n")
	err = genFile(ctx, params.Path, "constants/constants.go", genData)
	if err != nil {
		return err
	}

	// config
	fmt.Printf("[extension-scaffold] generating config\n")
	err = genFile(ctx, params.Path, "config/configvalues.go", genData)
	if err != nil {
		return err
	}
	err = genFile(ctx, params.Path, "config/extension.yaml", genData)
	if err != nil {
		return err
	}

	// translations
	fmt.Printf("[extension-scaffold] generating translations\n")
	err = genFile(ctx, params.Path, "translations/en.toml", genData)
	if err != nil {
		return err
	}
	err = genFile(ctx, params.Path, "translations/es.toml", genData)
	if err != nil {
		return err
	}

	// github workflows
	fmt.Printf("[extension-scaffold] generating github workflows\n")
	err = genFile(ctx, params.Path, ".github/workflows/go.yaml", genData)
	if err != nil {
		return err
	}
	err = genFile(ctx, params.Path, ".github/workflows/publish-image.yaml", genData)
	if err != nil {
		return err
	}

	// server
	fmt.Printf("[extension-scaffold] generating server\n")
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
	fmt.Printf("[extension-scaffold] go mod init\n")
	var out bytes.Buffer
	var stderr bytes.Buffer
	if !filetools.FileExists(path.Join(params.Path, "go.mod")) {
		cmd := exec.Command("go", "mod", "init", params.Module)
		cmd.Dir = params.Path
		cmd.Stdout = &out
		cmd.Stderr = &stderr
		err := cmd.Run()
		if err != nil {
			fmt.Printf("[extension-scaffold] error running go mod init: %v | %v | %v\n", err, out.String(), stderr.String())
		}
	} else {
		fmt.Printf("[extension-scaffold] go.mod already exists\n")
	}

	// go mod tidy
	fmt.Printf("[extension-scaffold] go mod tidy\n")
	cmd := exec.Command("go", "mod", "tidy")
	cmd.Dir = params.Path
	cmd.Stdout = &out
	cmd.Stderr = &stderr
	err = cmd.Run()
	if err != nil {
		fmt.Printf("[extension-scaffold] error running go mod init: %v | %v | %v\n", err, out.String(), stderr.String())
	}

	return nil
}

func genFile(ctx context.Context, path string, fileName string, genData GenData) error {
	isGoFile := strings.Contains(fileName, ".go")
	tmplBytes, err := templates.ReadFile(fmt.Sprintf("templates/%s.tmpl", fileName))
	if err != nil {
		return err
	}

	funcmap := template.FuncMap{
		"ToCamel": strcase.ToCamel,
		"ToCamelSingle": func(in string) string {
			pl := gopluralize.NewClient()
			ins := pl.Singular(in)
			return strcase.ToCamel(ins)
		},
	}
	_, err = filetools.GenerateFile(ctx, filetools.FileRequest{
		OutputPath:      filepath.Join(path, fileName),
		TemplateBytes:   tmplBytes,
		Data:            genData,
		DisableGoFormat: !isGoFile,
		Funcs:           funcmap,
	})
	if err != nil {
		return err
	}
	return nil
}
