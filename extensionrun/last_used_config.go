package extensionrun

import (
	"encoding/json"
	"fmt"
	"time"

	"github.com/nuzur/nuzur-cli/productclient"
	"github.com/nuzur/nuzur-cli/protodeps/gen"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// Last-used extension configs live in the user's project-version data under
// ExtensionsMetadata, keyed by extension identifier. The web app reads the same
// record, so the storage shape here is a cross-client contract.
//
// Paired extensions (sql-push / sql-push-local, sql-import / sql-import-local)
// each keep their own entry in that map: a run saves only the member that
// actually executed, in that backend's field shape, and both clients decide
// which mode to default to by comparing the two entries' lastUsed timestamps.
// That comparison is only meaningful if a save leaves the sibling's timestamp
// alone — hence the targeted upsert below.

// LastUsedEntry is one extension's saved config plus when it was last run.
type LastUsedEntry struct {
	ConfigValues map[string]interface{}
	// LastUsed is the zero time when absent or unparsable, which sorts oldest.
	LastUsed time.Time
}

// GetLastUsedConfigs returns the saved config per extension identifier for a
// project version, including each entry's lastUsed timestamp.
func (i *Implementation) GetLastUsedConfigs(projectVersionUUID string) (map[string]LastUsedEntry, error) {
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

	result := make(map[string]LastUsedEntry, len(pvd.ExtensionsMetadata))
	for identifier, meta := range pvd.ExtensionsMetadata {
		var configValues map[string]interface{}
		if meta.ConfigValues != "" {
			if err := json.Unmarshal([]byte(meta.ConfigValues), &configValues); err != nil {
				configValues = nil
			}
		}
		var lastUsed time.Time
		if meta.LastUsed != "" {
			// The web writes RFC3339 with a variable fractional part; a value we
			// can't parse just sorts oldest rather than failing the whole read.
			if parsed, err := time.Parse(time.RFC3339Nano, meta.LastUsed); err == nil {
				lastUsed = parsed
			}
		}
		result[identifier] = LastUsedEntry{ConfigValues: configValues, LastUsed: lastUsed}
	}

	return result, nil
}

// SaveLastUsedConfigEntry persists configValues for a single extension
// identifier, leaving every sibling entry and every other top-level key
// untouched.
func (i *Implementation) SaveLastUsedConfigEntry(projectVersionUUID, extensionIdentifier string, configValues map[string]interface{}) error {
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

	var existing []byte
	if err == nil && res.Data != "" {
		existing = []byte(res.Data)
	}

	data, err := upsertExtensionMetadata(existing, extensionIdentifier, configValues, time.Now().UTC())
	if err != nil {
		return err
	}

	_, err = i.productClient.ProductClient.SaveUserProjectVersionData(ctx, &gen.SaveUserProjectVersionDataRequest{
		ProjectVersionUuid: projectVersionUUID,
		Data:               string(data),
	})
	return err
}

// upsertExtensionMetadata rewrites only the entry for extensionIdentifier,
// stamping it with now. Sibling entries are copied through verbatim — keeping
// their lastUsed and extensionVersion — and unrelated top-level keys (e.g.
// DataManagerMetadata) are preserved as raw JSON.
func upsertExtensionMetadata(existingData []byte, extensionIdentifier string, configValues map[string]interface{}, now time.Time) ([]byte, error) {
	raw := make(map[string]json.RawMessage)
	if len(existingData) > 0 {
		if err := json.Unmarshal(existingData, &raw); err != nil {
			raw = make(map[string]json.RawMessage)
		}
	}

	extMeta := make(map[string]extensionMetadata)
	if encoded, ok := raw["ExtensionsMetadata"]; ok {
		if err := json.Unmarshal(encoded, &extMeta); err != nil {
			extMeta = make(map[string]extensionMetadata)
		}
	}

	cvBytes, err := json.Marshal(configValues)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal config values for %s: %w", extensionIdentifier, err)
	}

	entry := extMeta[extensionIdentifier]
	entry.LastUsed = now.Format(time.RFC3339Nano)
	entry.ConfigValues = string(cvBytes)
	entry.ExtensionIdentifier = extensionIdentifier
	extMeta[extensionIdentifier] = entry

	extMetaBytes, err := json.Marshal(extMeta)
	if err != nil {
		return nil, err
	}
	raw["ExtensionsMetadata"] = extMetaBytes

	return json.Marshal(raw)
}
