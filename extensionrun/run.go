package extensionrun

import (
	"archive/zip"
	"bytes"
	"context"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	extensiongen "github.com/nuzur/extension-sdk/idl/gen"
	nemgen "github.com/nuzur/nem/idl/gen"
	"github.com/nuzur/nuzur-cli/constants"
	"github.com/nuzur/nuzur-cli/files"
	"github.com/nuzur/nuzur-cli/productclient"
	"github.com/nuzur/nuzur-cli/protodeps/gen"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/metadata"
	"google.golang.org/protobuf/encoding/protojson"
)

type RunParams struct {
	Extension          *nemgen.Extension
	ExtensionVersion   *nemgen.ExtensionVersion
	ProjectUUID        string
	ProjectVersionUUID string
	ConfigValues       map[string]interface{}
	OutputPath         string
	// AutoConfirmSteps auto-approves CONFIRMATION steps (e.g. the SQL-diff
	// review in sql-push) so step-based extensions can run non-interactively.
	// Without it, a confirmation step is an error on the non-interactive path.
	AutoConfirmSteps bool
}

// RunResult is the structured outcome of an extension run, suitable for
// machine-readable (--json) output consumed by agents / MCP tooling.
type RunResult struct {
	Status        string   `json:"status"` // "succeeded"
	ExecutionUUID string   `json:"execution_uuid,omitempty"`
	OutputPath    string   `json:"output_path"`
	FilesWritten  []string `json:"files_written"`
	FilesRemoved  []string `json:"files_removed"`
}

func (i *Implementation) Run(params RunParams) (*RunResult, error) {
	// serialize config values as JSON string (the format extensions expect)
	configValuesBytes, err := json.Marshal(params.ConfigValues)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal config values: %w", err)
	}

	// build a gRPC client pointing at the extensions proxy
	extClient, conn, err := buildExtensionClient()
	if err != nil {
		return nil, fmt.Errorf("failed to connect to extension: %w", err)
	}
	defer conn.Close()

	// build context with the bearer token and extension identifier
	tokenBytes, err := os.ReadFile(files.TokenFilePath())
	if err != nil {
		return nil, fmt.Errorf("failed to read auth token: %w", err)
	}
	ctx := metadata.NewOutgoingContext(
		contextWithTimeout(30),
		metadata.New(map[string]string{
			"authorization": fmt.Sprintf("bearer %s", string(tokenBytes)),
			"extension":     params.Extension.Identifier,
		}),
	)

	fmt.Fprintln(os.Stderr, "Starting extension execution...")
	resp, err := extClient.StartExecution(ctx, &extensiongen.StartExecutionRequest{
		ProjectUuid:        params.ProjectUUID,
		ProjectVersionUuid: params.ProjectVersionUUID,
		ConfigValues:       string(configValuesBytes),
	})
	if err != nil {
		return nil, fmt.Errorf("failed to start extension execution: %w", err)
	}

	fmt.Fprintf(os.Stderr, "Extension execution started (uuid: %s)\n", resp.ExecutionUuid)

	switch resp.Type {
	case extensiongen.ExecutionResponseType_EXECUTION_RESPONSE_TYPE_FINAL:
		// synchronous — result is already available
		return i.handleFinalResponse(resp.Data.Final, params.OutputPath, resp.ExecutionUuid)

	case extensiongen.ExecutionResponseType_EXECUTION_RESPONSE_TYPE_ASYNC,
		extensiongen.ExecutionResponseType_EXECUTION_RESPONSE_TYPE_STEP:
		// async or step-based — poll the extension server until done, handling
		// any confirmation steps along the way.
		if resp.Data != nil && resp.Data.Async != nil && resp.Data.Async.StatusMessage != "" {
			fmt.Fprintf(os.Stderr, "Async execution: %s\n", resp.Data.Async.StatusMessage)
		}
		return i.pollExtensionExecution(extClient, tokenBytes, params.Extension.Identifier, resp.ExecutionUuid, params.OutputPath, params.AutoConfirmSteps)

	default:
		return nil, fmt.Errorf("unsupported execution response type: %v", resp.Type)
	}
}

func (i *Implementation) pollExtensionExecution(
	extClient extensiongen.NuzurExtensionClient,
	tokenBytes []byte,
	extensionIdentifier string,
	executionUUID string,
	outputPath string,
	autoConfirm bool,
) (*RunResult, error) {
	lastStatus := ""
	submitted := map[string]bool{} // confirmation steps already answered
	for {
		ctx := metadata.NewOutgoingContext(
			contextWithTimeout(30),
			metadata.New(map[string]string{
				"authorization": fmt.Sprintf("bearer %s", string(tokenBytes)),
				"extension":     extensionIdentifier,
			}),
		)

		exec, err := extClient.GetExecution(ctx, &extensiongen.GetExecutionRequest{
			ExecutionUuid: executionUUID,
		})
		if err != nil {
			return nil, fmt.Errorf("failed to poll execution status: %w", err)
		}

		switch exec.Status {
		case extensiongen.ExecutionStatus_EXECUTION_STATUS_SUCCEEDED:
			fmt.Fprintln(os.Stderr, "Extension execution succeeded, fetching output...")
			if exec.Data != nil && exec.Data.Final != nil {
				return i.handleFinalResponse(exec.Data.Final, outputPath, executionUUID)
			}
			return nil, errors.New("execution succeeded but no final data returned")
		case extensiongen.ExecutionStatus_EXECUTION_STATUS_FAILED:
			return nil, fmt.Errorf("extension execution failed: %s", exec.StatusMsg)
		case extensiongen.ExecutionStatus_EXECUTION_STATUS_CANCELLED:
			return nil, errors.New("execution was cancelled")
		case extensiongen.ExecutionStatus_EXECUTION_STATUS_INPROGRESS:
			// A CONFIRMATION step blocks until answered — auto-confirm it (once)
			// so step-based extensions complete non-interactively.
			if exec.Type == extensiongen.ExecutionResponseType_EXECUTION_RESPONSE_TYPE_STEP &&
				exec.Data != nil && exec.Data.Step != nil &&
				exec.Data.Step.Type == extensiongen.ExecutionStepType_EXECUTION_STEP_TYPE_CONFIRMATION {
				stepID := exec.Data.Step.StepIdentifier
				if !submitted[stepID] {
					if !autoConfirm {
						return nil, fmt.Errorf("extension is waiting on confirmation step %q and this run is non-interactive; enable step auto-confirmation to proceed", stepID)
					}
					fmt.Fprintf(os.Stderr, "Auto-confirming step: %s\n", stepID)
					if err := i.submitStep(extClient, tokenBytes, extensionIdentifier, executionUUID, stepID); err != nil {
						return nil, err
					}
					submitted[stepID] = true
				}
			}
			msg := exec.StatusMsg
			if exec.CurrentStepIdentifier != "" {
				msg = fmt.Sprintf("%s (step: %s)", msg, exec.CurrentStepIdentifier)
			}
			if msg != lastStatus {
				fmt.Fprintf(os.Stderr, "Execution in progress: %s\n", msg)
				lastStatus = msg
			}
		}

		time.Sleep(500 * time.Millisecond)

		// re-read token in case it was refreshed
		newToken, err := os.ReadFile(files.TokenFilePath())
		if err == nil {
			tokenBytes = newToken
		}
	}
}

// submitStep answers a confirmation step, confirming it so the extension can
// proceed (used for non-interactive step-based runs like SQL push).
func (i *Implementation) submitStep(extClient extensiongen.NuzurExtensionClient, tokenBytes []byte, extensionIdentifier, executionUUID, stepID string) error {
	ctx := metadata.NewOutgoingContext(
		contextWithTimeout(30),
		metadata.New(map[string]string{
			"authorization": fmt.Sprintf("bearer %s", string(tokenBytes)),
			"extension":     extensionIdentifier,
		}),
	)
	if _, err := extClient.SubmitExectuionStep(ctx, &extensiongen.SubmitExectuionStepRequest{
		ExecutionUuid:  executionUUID,
		StepIdentifier: stepID,
		Confirmed:      true,
	}); err != nil {
		return fmt.Errorf("submitting confirmation step %q: %w", stepID, err)
	}
	return nil
}

func (i *Implementation) handleFinalResponse(final *extensiongen.ExecutionResponseTypeFinalData, outputPath, executionUUID string) (*RunResult, error) {
	if final == nil {
		return nil, errors.New("no final data in execution response")
	}
	if final.Status != extensiongen.ExecutionStatus_EXECUTION_STATUS_SUCCEEDED {
		return nil, fmt.Errorf("execution failed: %s", final.StatusMessage)
	}
	if final.FileDownloadUrl == "" {
		// Non-generator extensions (e.g. SQL push) produce no downloadable file;
		// a successful terminal status is the outcome.
		return &RunResult{Status: "succeeded", ExecutionUUID: executionUUID, OutputPath: outputPath}, nil
	}

	ctx, err := productclient.ClientContext()
	if err != nil {
		return nil, fmt.Errorf("failed to build product client context: %w", err)
	}
	signedRes, err := i.productClient.ProductClient.GetSignedFileURL(ctx, &gen.GetSignedFileURLRequest{
		Url: final.FileDownloadUrl,
	})
	if err != nil {
		return nil, fmt.Errorf("failed to get signed file URL: %w", err)
	}

	written, removed, err := i.downloadAndExtract(signedRes.Url, outputPath)
	if err != nil {
		return nil, err
	}
	return &RunResult{
		Status:        "succeeded",
		ExecutionUUID: executionUUID,
		OutputPath:    outputPath,
		FilesWritten:  written,
		FilesRemoved:  removed,
	}, nil
}

func buildExtensionClient() (extensiongen.NuzurExtensionClient, *grpc.ClientConn, error) {
	pool, err := x509.SystemCertPool()
	if err != nil {
		return nil, nil, fmt.Errorf("failed to load system cert pool: %w", err)
	}
	creds := credentials.NewClientTLSFromCert(pool, "")
	conn, err := grpc.NewClient(constants.EXTENSIONS_PROXY_ADDRESS, grpc.WithTransportCredentials(creds))
	if err != nil {
		return nil, nil, fmt.Errorf("failed to dial extensions proxy: %w", err)
	}
	return extensiongen.NewNuzurExtensionClient(conn), conn, nil
}

// downloadAndExtract fetches the generated zip and writes it under outputPath,
// returning the relative paths written and any stale files removed.
func (i *Implementation) downloadAndExtract(signedURL string, outputPath string) ([]string, []string, error) {
	resp, err := http.Get(signedURL) // #nosec G107 - URL comes from trusted extension server
	if err != nil {
		return nil, nil, fmt.Errorf("failed to download execution file: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, nil, fmt.Errorf("failed to download execution file: HTTP %d", resp.StatusCode)
	}

	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to read execution file: %w", err)
	}

	if err := os.MkdirAll(outputPath, 0750); err != nil {
		return nil, nil, fmt.Errorf("failed to create output directory: %w", err)
	}

	// Capture the previous generation's manifest before the new output overwrites
	// it, so we can detect files that are no longer produced.
	previousManifest, hadPrevious, err := files.ReadGeneratedManifest(outputPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Warning: could not read previous generation manifest: %v\n", err)
	}

	written, err := extractZip(data, outputPath)
	if err != nil {
		return nil, nil, err
	}

	removed := cleanupOrphanedGeneratedFiles(outputPath, previousManifest, hadPrevious)
	return written, removed, nil
}

// cleanupOrphanedGeneratedFiles removes files generated by a previous run that
// the current run no longer produces, leaving user-added files untouched. It is
// driven by the presence of a generation manifest, so any extension that emits
// one benefits; extensions that don't are unaffected.
func cleanupOrphanedGeneratedFiles(outputPath string, previousManifest files.GeneratedManifest, hadPrevious bool) []string {
	if !hadPrevious {
		return nil // first run with a manifest (or generator without one): nothing to compare against
	}

	currentManifest, hasCurrent, err := files.ReadGeneratedManifest(outputPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Warning: could not read current generation manifest: %v\n", err)
		return nil
	}
	if !hasCurrent {
		return nil // current run did not emit a manifest; do not delete anything
	}

	removed, err := files.CleanupOrphanedGeneratedFiles(outputPath, previousManifest, currentManifest)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Warning: failed to clean up stale generated files: %v\n", err)
	}
	if len(removed) > 0 {
		fmt.Fprintf(os.Stderr, "Removed %d stale generated file(s):\n", len(removed))
		for _, r := range removed {
			fmt.Fprintf(os.Stderr, "  - %s\n", r)
		}
	}
	return removed
}

func contextWithTimeout(seconds int) context.Context {
	ctx, cancel := context.WithTimeout(context.Background(), time.Duration(seconds)*time.Second)
	_ = cancel // timeout will free resources; cancel is intentionally discarded for single-call contexts
	return ctx
}

// extractZip writes every file in the archive under outputPath and returns the
// slash-separated relative paths written, in archive order.
func extractZip(data []byte, outputPath string) ([]string, error) {
	r, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		return nil, fmt.Errorf("failed to open zip archive: %w", err)
	}

	var written []string
	for _, f := range r.File {
		destPath, err := sanitizeZipPath(outputPath, f.Name)
		if err != nil {
			return nil, err
		}

		if f.FileInfo().IsDir() {
			if err := os.MkdirAll(destPath, 0750); err != nil {
				return nil, fmt.Errorf("failed to create directory %s: %w", destPath, err)
			}
			continue
		}

		if err := os.MkdirAll(filepath.Dir(destPath), 0750); err != nil {
			return nil, fmt.Errorf("failed to create parent directory: %w", err)
		}

		if err := writeZipFile(f, destPath); err != nil {
			return nil, err
		}
		written = append(written, filepath.ToSlash(filepath.Clean(f.Name)))
	}

	return written, nil
}

// sanitizeZipPath prevents zip-slip path traversal attacks
func sanitizeZipPath(base, name string) (string, error) {
	destPath := filepath.Join(base, name)
	absBase, err := filepath.Abs(base)
	if err != nil {
		return "", err
	}
	absDest, err := filepath.Abs(destPath)
	if err != nil {
		return "", err
	}
	if len(absDest) < len(absBase) || absDest[:len(absBase)] != absBase {
		return "", fmt.Errorf("invalid zip entry path: %s", name)
	}
	return destPath, nil
}

func writeZipFile(f *zip.File, destPath string) error {
	rc, err := f.Open()
	if err != nil {
		return fmt.Errorf("failed to open zip entry %s: %w", f.Name, err)
	}
	defer rc.Close()

	outFile, err := os.OpenFile(destPath, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, f.Mode())
	if err != nil {
		return fmt.Errorf("failed to create file %s: %w", destPath, err)
	}
	defer outFile.Close()

	if _, err := io.Copy(outFile, rc); err != nil { // #nosec G110 - zip from trusted API
		return fmt.Errorf("failed to write file %s: %w", destPath, err)
	}

	return nil
}

type extensionMetadata struct {
	LastUsed            string `json:"lastUsed"`
	ConfigValues        string `json:"configValues"`
	ExtensionVersion    string `json:"extensionVersion"`
	ExtensionIdentifier string `json:"extensionIdentifier"`
}

type projectVersionData struct {
	ExtensionsMetadata map[string]extensionMetadata `json:"ExtensionsMetadata"`
}

// GetLastUsedConfig returns a map of extensionIdentifier -> configValues (parsed map) from the stored project version data.
func (i *Implementation) GetLastUsedConfig(projectVersionUUID string) (map[string]map[string]interface{}, error) {
	ctx, err := productclient.ClientContext()
	if err != nil {
		return nil, err
	}

	res, err := i.productClient.ProductClient.GetUserProjectVersionData(ctx, &gen.GetUserProjectVersionDataRequest{
		ProjectVersionUuid: projectVersionUUID,
	})

	if err != nil {
		return nil, err
	}

	if res.Data == "" {
		return nil, nil
	}

	var pvd projectVersionData
	if err := json.Unmarshal([]byte(res.Data), &pvd); err != nil {
		return nil, nil
	}

	if len(pvd.ExtensionsMetadata) == 0 {
		return nil, nil
	}

	result := make(map[string]map[string]interface{}, len(pvd.ExtensionsMetadata))
	for identifier, meta := range pvd.ExtensionsMetadata {
		var configValues map[string]interface{}
		if meta.ConfigValues != "" {
			if err := json.Unmarshal([]byte(meta.ConfigValues), &configValues); err != nil {
				configValues = nil
			}
		}
		result[identifier] = configValues
	}

	return result, nil
}

// SaveLastUsedConfig persists configValues per extension identifier into the ExtensionsMetadata field,
// preserving any other top-level keys (e.g. DataManagerMetadata) already in the stored data.
func (i *Implementation) SaveLastUsedConfig(projectVersionUUID string, configs map[string]map[string]interface{}) error {
	ctx, err := productclient.ClientContext()
	if err != nil {
		return err
	}

	// fetch existing data to preserve other keys; NotFound means no record yet, which is fine
	res, err := i.productClient.ProductClient.GetUserProjectVersionData(ctx, &gen.GetUserProjectVersionDataRequest{
		ProjectVersionUuid: projectVersionUUID,
	})
	if err != nil && status.Code(err) != codes.NotFound {
		return err
	}

	// unmarshal into a generic map so we don't clobber other top-level keys
	raw := make(map[string]json.RawMessage)
	if err == nil && res.Data != "" {
		if err := json.Unmarshal([]byte(res.Data), &raw); err != nil {
			raw = make(map[string]json.RawMessage)
		}
	}

	now := time.Now().UTC().Format(time.RFC3339Nano)
	extMeta := make(map[string]extensionMetadata, len(configs))
	for identifier, configValues := range configs {
		cvBytes, err := json.Marshal(configValues)
		if err != nil {
			return fmt.Errorf("failed to marshal config values for %s: %w", identifier, err)
		}
		extMeta[identifier] = extensionMetadata{
			LastUsed:            now,
			ConfigValues:        string(cvBytes),
			ExtensionIdentifier: identifier,
		}
	}

	extMetaBytes, err := json.Marshal(extMeta)
	if err != nil {
		return err
	}
	raw["ExtensionsMetadata"] = extMetaBytes

	data, err := json.Marshal(raw)
	if err != nil {
		return err
	}

	_, err = i.productClient.ProductClient.SaveUserProjectVersionData(ctx, &gen.SaveUserProjectVersionDataRequest{
		ProjectVersionUuid: projectVersionUUID,
		Data:               string(data),
	})
	return err
}

func (i *Implementation) GetConfigEntity(extensionVersion *nemgen.ExtensionVersion) (*extensiongen.ExtensionConfigurationEntity, error) {
	configEntity := &extensiongen.ExtensionConfigurationEntity{}
	if extensionVersion.ConfigurationEntity == "" {
		return configEntity, nil
	}
	if err := protojson.Unmarshal([]byte(extensionVersion.ConfigurationEntity), configEntity); err != nil {
		return nil, fmt.Errorf("failed to parse extension config entity: %w", err)
	}
	return configEntity, nil
}
