package extensionscaffold

import (
	"bufio"
	"errors"
	"fmt"
	"os"
	"path"
	"strings"

	extensiongen "github.com/nuzur/extension-sdk/idl/gen"
	"github.com/nuzur/nuzur-cli/productclient"
	"github.com/nuzur/nuzur-cli/protodeps/gen"
	"google.golang.org/protobuf/encoding/protojson"
)

type ExtensionUpdateConfigParams struct {
	ExtensionUUID        string
	ExtensionVersionUUID string
	Path                 string
}

func (i *Implementation) ExtensionUpdateConfig(params ExtensionUpdateConfigParams) error {
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

	// read existing mod file to get the module path
	file, err := os.Open(path.Join(params.Path, "go.mod"))
	if err != nil {
		return err
	}
	defer file.Close()

	reader := bufio.NewReader(file)
	line, _, err := reader.ReadLine()
	if err != nil {
		return err
	}

	if !strings.Contains(string(line), "module") {
		return errors.New("go.mod file does not contain module definition")
	}

	module := strings.Split(string(line), " ")[1]
	if module == "" {
		return errors.New("module definition is empty")
	}

	genData := GenData{
		ModulePath:       module,
		Extension:        extension,
		ExtensionVersion: extensionVersion,
		ConfigEntity:     &extensionConfigEntity,
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

	return nil

}
