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
	"strings"
	"time"

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
	// OnConfirmationStep decides each CONFIRMATION step, seeing its payload first.
	// When nil the run falls back to AutoConfirmSteps — confirm everything, or
	// refuse to proceed — which is what `run-extension --confirm-steps` relies on,
	// so leaving this unset preserves the pre-existing behavior exactly.
	OnConfirmationStep StepDecider
}

// RunResult is the structured outcome of an extension run, suitable for
// machine-readable (--json) output consumed by agents / MCP tooling.
type RunResult struct {
	Status        string   `json:"status"` // "succeeded" | "cancelled"
	ExecutionUUID string   `json:"execution_uuid,omitempty"`
	OutputPath    string   `json:"output_path"`
	FilesWritten  []string `json:"files_written"`
	FilesRemoved  []string `json:"files_removed"`
	// StatusMessage is the extension's terminal message. It used to be discarded,
	// which is why sql-push's "No changes to apply" — the single most useful thing
	// it says — had never reached a user.
	StatusMessage string `json:"status_message,omitempty"`
	// Steps records every confirmation step and how it was answered, so a step's
	// payload outlives the poll loop.
	Steps []StepOutcome `json:"steps,omitempty"`
	// DisplayBlocks are the terminal response's blocks (e.g. sql-gen's rendered
	// SQL), previously discarded along with everything else non-file.
	DisplayBlocks []DisplayBlock `json:"display_blocks,omitempty"`
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
		return i.handleFinalResponse(resp.Data.Final, params.OutputPath, resp.ExecutionUuid, nil)

	case extensiongen.ExecutionResponseType_EXECUTION_RESPONSE_TYPE_ASYNC,
		extensiongen.ExecutionResponseType_EXECUTION_RESPONSE_TYPE_STEP:
		// async or step-based — poll the extension server until done, handling
		// any confirmation steps along the way.
		if async := resp.GetData().GetAsync(); async != nil {
			if async.Queued {
				fmt.Fprintln(os.Stderr, queueWaitingLine(async))
			} else if async.StatusMessage != "" {
				fmt.Fprintf(os.Stderr, "Async execution: %s\n", async.StatusMessage)
			}
		}
		return i.pollExtensionExecution(extClient, tokenBytes, params, resp.ExecutionUuid)

	default:
		return nil, fmt.Errorf("unsupported execution response type: %v", resp.Type)
	}
}

func (i *Implementation) pollExtensionExecution(
	extClient extensiongen.NuzurExtensionClient,
	tokenBytes []byte,
	params RunParams,
	executionUUID string,
) (*RunResult, error) {
	extensionIdentifier := params.Extension.Identifier
	outputPath := params.OutputPath
	decide := params.stepDecider()

	lastStatus := ""
	submitted := map[string]bool{} // confirmation steps already answered
	var steps []StepOutcome        // every step and how it was answered
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
				return i.handleFinalResponse(exec.Data.Final, outputPath, executionUUID, steps)
			}
			return nil, errors.New("execution succeeded but no final data returned")
		case extensiongen.ExecutionStatus_EXECUTION_STATUS_FAILED:
			return nil, fmt.Errorf("extension execution failed: %s", exec.StatusMsg)
		case extensiongen.ExecutionStatus_EXECUTION_STATUS_CANCELLED:
			// Terminal cancelled. A rejected confirmation step lands here, so the
			// result is returned POPULATED alongside the sentinel error: a caller
			// that rejected on purpose needs the payload it rejected, and one that
			// did not still sees an error and fails as it always has.
			return &RunResult{
				Status:        "cancelled",
				ExecutionUUID: executionUUID,
				OutputPath:    outputPath,
				StatusMessage: exec.StatusMsg,
				Steps:         steps,
			}, ErrExecutionCancelled
		case extensiongen.ExecutionStatus_EXECUTION_STATUS_INPROGRESS:
			// A CONFIRMATION step blocks until answered. The decider sees the step's
			// payload — for sql-push, the migration itself — and says yes or no.
			if exec.Type == extensiongen.ExecutionResponseType_EXECUTION_RESPONSE_TYPE_STEP &&
				exec.Data != nil && exec.Data.Step != nil &&
				exec.Data.Step.Type == extensiongen.ExecutionStepType_EXECUTION_STEP_TYPE_CONFIRMATION {
				stepID := exec.Data.Step.StepIdentifier
				if !submitted[stepID] {
					prompt := stepPromptFromStep(exec.Data.Step)
					decision, err := decide(prompt)
					if err != nil {
						return nil, err
					}
					if err := i.submitStep(extClient, tokenBytes, extensionIdentifier, executionUUID, stepID, decision.Confirm); err != nil {
						return nil, err
					}
					// Marked for BOTH answers: once the extension has moved to a
					// terminal status it clears the current step, so submitting
					// again fails with InvalidArgument.
					submitted[stepID] = true
					steps = append(steps, StepOutcome{Prompt: prompt, Confirmed: decision.Confirm, Reason: decision.Reason})
				}
			}
			// While waiting in the admission queue the extension reports an
			// in-progress status carrying structured queue info; render it as a
			// dedicated "waiting" line rather than as active progress.
			if async := exec.GetData().GetAsync(); async != nil && async.Queued {
				if line := queueWaitingLine(async); line != lastStatus {
					fmt.Fprintln(os.Stderr, line)
					lastStatus = line
				}
				break
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

// submitStep answers a confirmation step.
//
// confirmed=false is a real answer, not an abort: the extension takes it as a
// rejection and ends the execution having done nothing. That is what makes a dry
// run possible for an extension with no dry-run mode of its own.
func (i *Implementation) submitStep(extClient extensiongen.NuzurExtensionClient, tokenBytes []byte, extensionIdentifier, executionUUID, stepID string, confirmed bool) error {
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
		Confirmed:      confirmed,
	}); err != nil {
		return fmt.Errorf("submitting confirmation step %q: %w", stepID, err)
	}
	return nil
}

func (i *Implementation) handleFinalResponse(final *extensiongen.ExecutionResponseTypeFinalData, outputPath, executionUUID string, steps []StepOutcome) (*RunResult, error) {
	if final == nil {
		return nil, errors.New("no final data in execution response")
	}
	if final.Status != extensiongen.ExecutionStatus_EXECUTION_STATUS_SUCCEEDED {
		return nil, fmt.Errorf("execution failed: %s", final.StatusMessage)
	}
	if final.FileDownloadUrl == "" {
		// Non-generator extensions (e.g. SQL push) produce no downloadable file;
		// the terminal status and its message and blocks are the outcome.
		return &RunResult{
			Status:        "succeeded",
			ExecutionUUID: executionUUID,
			OutputPath:    outputPath,
			StatusMessage: final.StatusMessage,
			Steps:         steps,
			DisplayBlocks: displayBlocksFrom(final.DisplayBlocks),
		}, nil
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
		StatusMessage: final.StatusMessage,
		Steps:         steps,
		DisplayBlocks: displayBlocksFrom(final.DisplayBlocks),
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

	return applyGeneratedArchive(data, outputPath)
}

// applyGeneratedArchive writes the archive into outputPath and prunes files a
// previous generation produced that this one no longer does. It is the whole
// extract+cleanup pipeline, separated from the HTTP download so tests exercise
// the real wiring (root resolution included) rather than a copy of it.
func applyGeneratedArchive(data []byte, outputPath string) ([]string, []string, error) {
	if err := os.MkdirAll(outputPath, 0750); err != nil {
		return nil, nil, fmt.Errorf("failed to create output directory: %w", err)
	}

	// Capture the previous generation's manifest before the new output overwrites
	// it, so we can detect files that are no longer produced. The manifest sits at
	// the generated project's own root, which is usually a directory BELOW
	// outputPath, so it has to be located rather than assumed.
	previousRoot, previousManifest, hadPrevious, err := files.FindGeneratedManifest(outputPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Warning: could not read previous generation manifest: %v\n", err)
	}

	// The archive's go.mod overwrites the workspace's (see generatorManagedFiles),
	// so the directives only the local copy can know are snapshotted while it is
	// still there and re-applied below. See gomod.go for what and why.
	localEdits := snapshotLocalModuleEdits(outputPath)

	written, err := extractZip(data, outputPath)
	if err != nil {
		return nil, nil, err
	}

	removed := cleanupOrphanedGeneratedFiles(outputPath, previousRoot, previousManifest, hadPrevious)

	// LAST, and deliberately so: tidy must see the workspace exactly as the
	// developer will — generated code written, user-owned files preserved, stale
	// generated files already pruned — because it derives the module's requires
	// from the imports it finds on disk. Best-effort; warns, never fails.
	reconcileWorkspaceModule(outputPath, localEdits)
	return written, removed, nil
}

// cleanupOrphanedGeneratedFiles removes files generated by a previous run that
// the current run no longer produces, leaving user-added files untouched. It is
// driven by the presence of a generation manifest, so any extension that emits
// one benefits; extensions that don't are unaffected.
func cleanupOrphanedGeneratedFiles(outputPath, previousRoot string, previousManifest files.GeneratedManifest, hadPrevious bool) []string {
	if !hadPrevious {
		return nil // first run with a manifest (or generator without one): nothing to compare against
	}

	currentRoot, currentManifest, hasCurrent, err := files.FindGeneratedManifest(outputPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Warning: could not read current generation manifest: %v\n", err)
		return nil
	}
	if !hasCurrent {
		return nil // current run did not emit a manifest; do not delete anything
	}
	// Both manifests must describe the SAME project root for a diff between them
	// to mean anything. They differ when the generated root was renamed (a changed
	// identifier), which leaves the old tree as a separate project rather than as
	// stale files — not ours to delete.
	if currentRoot != previousRoot {
		fmt.Fprintf(os.Stderr,
			"Warning: the generated project root moved (%s -> %s); skipping stale-file cleanup. The previous output is still in %s and can be removed by hand.\n",
			previousRoot, currentRoot, previousRoot)
		return nil
	}

	removed, err := files.CleanupOrphanedGeneratedFiles(currentRoot, previousManifest, currentManifest)
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

// queueWaitingLine renders the "waiting in the generation queue" status shown
// to the user while an execution is admitted-pending, using the structured
// queue fields the extension server reports.
func queueWaitingLine(async *extensiongen.ExecutionResponseTypeAsyncData) string {
	if async.QueuePosition <= 0 {
		return "Waiting for a generation slot…"
	}
	if async.EstimatedWaitSeconds > 0 {
		return fmt.Sprintf("Waiting for a generation slot — position %d (about %s)…",
			async.QueuePosition, humanizeSeconds(async.EstimatedWaitSeconds))
	}
	return fmt.Sprintf("Waiting for a generation slot — position %d…", async.QueuePosition)
}

// humanizeSeconds renders a rough wait as a compact human string.
func humanizeSeconds(s int64) string {
	if s < 60 {
		return fmt.Sprintf("%ds", s)
	}
	m := (s + 59) / 60
	return fmt.Sprintf("%dm", m)
}

func contextWithTimeout(seconds int) context.Context {
	ctx, cancel := context.WithTimeout(context.Background(), time.Duration(seconds)*time.Second)
	_ = cancel // timeout will free resources; cancel is intentionally discarded for single-call contexts
	return ctx
}

// extractZip writes the archive under outputPath and returns the slash-separated
// relative paths written.
//
// When the archive carries a generation manifest it follows the
// generated-marker convention, so extraction PRESERVES user-owned files: a file
// that already exists locally and whose incoming entry is neither the manifest
// nor a generated (marked) file is left untouched. This is what lets a persistent
// workspace keep the user's custom app zone (app/grpc.go, app/rest.go,
// custom.proto) across re-generations — generation runs server-side and the zip
// always contains empty custom stubs, so the client must not clobber local edits.
// Archives without a manifest keep the simple overwrite-everything behavior.
func extractZip(data []byte, outputPath string) ([]string, error) {
	r, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		return nil, fmt.Errorf("failed to open zip archive: %w", err)
	}

	preserveUserFiles := zipHasManifest(r)

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

		// Preserve an existing user-owned file: the manifest, generated (marked)
		// files and the generator-managed module files are always refreshed;
		// anything else that already exists locally is left as the user edited it.
		if preserveUserFiles &&
			filepath.Base(f.Name) != files.GeneratedManifestFileName &&
			!isGeneratorManagedFile(f.Name) &&
			!zipEntryHasGeneratedMarker(f) &&
			regularFileExists(destPath) {
			continue
		}

		if err := writeZipFile(f, destPath); err != nil {
			return nil, err
		}
		written = append(written, filepath.ToSlash(filepath.Clean(f.Name)))
	}

	return written, nil
}

// generatorManagedFiles are refreshed from the archive even though they carry no
// generated marker.
//
// go.mod and go.sum cannot carry one — a Go module file has no comment convention
// the toolchain would tolerate at the top — so the marker rule classified them as
// user-owned and preserved them forever. They are not user-owned: the generator runs
// `go mod tidy` server-side, so the archive's copies are the authoritative answer for
// the code it just generated. Keeping the local ones meant that when generated code
// picked up a new third-party import, the workspace stopped building locally (`no
// required module provides package …`) and was rescued only by the `go mod tidy` in
// the on-box docker build — so it deployed fine and failed on the developer's machine,
// which is the worst possible split.
//
// The archive's copies are NOT the whole answer, though, and the comment that used to
// sit here claimed they were. Generation runs remotely against a tree that has no copy
// of the user's `app/` zone, so the generator tidies against generated code ALONE: a
// require that exists only because user-owned code imports it (golang.org/x/time/rate
// in the reported case) is absent from the archive's go.mod, correctly from the
// generator's point of view and wrongly from the workspace's. Nothing in this CLI then
// re-derived it — the claim that "the same `go mod tidy` the workspace runs" recovers
// it described a run that did not exist; the only tidy that actually happened was the
// one in the generated Dockerfile, during the image build, which is exactly why the
// deploy and the container stayed healthy while `go build ./...` failed on the
// developer's machine.
//
// What restores it now is reconcileWorkspaceModule (gomod.go), which runs `go mod tidy`
// in the workspace AFTER extraction — with the preserved user-owned files on disk — and
// first re-applies the local `replace`/`exclude`/`tool`/`godebug` directives that no
// tidy could re-derive.
var generatorManagedFiles = map[string]bool{
	"go.mod": true,
	"go.sum": true,
}

// isGeneratorManagedFile reports whether a zip entry names one of the files above.
func isGeneratorManagedFile(name string) bool {
	return generatorManagedFiles[filepath.Base(name)]
}

// zipHasManifest reports whether the archive contains a generation manifest,
// which signals it follows the generated-marker convention (and thus supports
// user-owned-file preservation).
func zipHasManifest(r *zip.Reader) bool {
	for _, f := range r.File {
		if filepath.Base(f.Name) == files.GeneratedManifestFileName {
			return true
		}
	}
	return false
}

// zipEntryHasGeneratedMarker reports whether a zip entry's content carries the
// "Code generated ... DO NOT EDIT" marker (mirrors files.IsGeneratedFile, but on
// the incoming archive entry rather than a file on disk).
func zipEntryHasGeneratedMarker(f *zip.File) bool {
	rc, err := f.Open()
	if err != nil {
		return false
	}
	defer rc.Close()
	buf := make([]byte, 512)
	n, _ := io.ReadFull(rc, buf) // short read is fine; the marker sits at the top
	head := string(buf[:n])
	return strings.Contains(head, "Code generated") && strings.Contains(head, "DO NOT EDIT")
}

// regularFileExists reports whether path exists and is a regular file.
func regularFileExists(path string) bool {
	info, err := os.Stat(path)
	return err == nil && info.Mode().IsRegular()
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

// Reading and writing these entries lives in last_used_config.go.

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
