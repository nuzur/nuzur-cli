// Package connections holds the agent's local registry of database
// connections — the set of named (name, driver, DSN, db_type) entries the user
// has added via `nuzur-cli agent connection add`.
//
// Two faces:
//   - On-disk persistence at /tmp/nuzur-cli/local_agent_connections.json, with
//     0600 perms. Plaintext for v1; phase-3 follow-up moves DSNs to the OS
//     keychain (only metadata + a keychain key stays here).
//   - In-process lookup by uuid for the daemon's RunQuery handler. The daemon
//     calls Load() once at startup and re-loads on SIGHUP (todo).
//
// The CLI publishes a *non-secret* slice of this to the cloud via
// UpdateLocalAgentConnections. The cloud never sees DSNs.
package connections

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path"
	"strings"

	"github.com/gofrs/uuid"
	"github.com/nuzur/nuzur-cli/files"
)

// DBType mirrors nem's LocalAgentConnectionDbType enum so we can serialize
// the catalog without depending on the protobuf package at this layer.
// 0=INVALID, 1=MYSQL, 2=POSTGRES.
type DBType int32

const (
	DBTypeInvalid  DBType = 0
	DBTypeMySQL    DBType = 1
	DBTypePostgres DBType = 2
)

// Entry is a single registered connection. DSN is intentionally NOT a JSON
// field — it lives in the OS keychain keyed by UUID. The runtime value is
// populated by FindByUUID / FindByName via a keychain lookup.
type Entry struct {
	UUID          string `json:"uuid"`
	Name          string `json:"name"`
	Driver        string `json:"driver"`         // mysql | postgres (matches sql.Open driver name)
	DBType        DBType `json:"db_type"`        // mirrors nem.LocalAgentConnectionDbType
	DSN           string `json:"-"`              // resolved from keychain at lookup time
	DefaultSchema string `json:"default_schema"` // optional

	// Legacy plaintext DSN field. Only set when parsing a registry file
	// written before the keychain migration. Load() drains this into the
	// keychain and zeroes it before returning the registry. Not present in
	// freshly written files.
	LegacyDSN string `json:"dsn,omitempty"`
}

// Registry is the in-memory snapshot of all locally-registered connections.
type Registry struct {
	Entries []Entry `json:"entries"`
}

// Load reads the on-disk registry. Returns an empty Registry if the file
// doesn't exist yet (first run). If the file contains legacy plaintext DSNs
// (from before the keychain migration), they're moved to the OS keychain
// and the registry is rewritten before returning — so callers don't see
// plaintext DSNs and the legacy file shape converts in place.
func Load() (*Registry, error) {
	b, err := os.ReadFile(files.LocalAgentConnectionsFilePath())
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return &Registry{}, nil
		}
		return nil, fmt.Errorf("read connections file: %w", err)
	}
	var r Registry
	if err := json.Unmarshal(b, &r); err != nil {
		return nil, fmt.Errorf("parse connections file: %w", err)
	}

	// Migrate legacy plaintext DSNs to the keychain. We only rewrite the
	// file when at least one entry needed migration so we don't churn
	// timestamps on every Load.
	migrated := false
	for i := range r.Entries {
		if r.Entries[i].LegacyDSN != "" {
			if err := PutDSN(r.Entries[i].UUID, r.Entries[i].LegacyDSN); err != nil {
				return nil, fmt.Errorf("migrating legacy DSN for %q: %w", r.Entries[i].Name, err)
			}
			r.Entries[i].DSN = r.Entries[i].LegacyDSN
			r.Entries[i].LegacyDSN = ""
			migrated = true
		}
	}
	if migrated {
		if err := r.Save(); err != nil {
			return nil, fmt.Errorf("rewrite registry after legacy migration: %w", err)
		}
	}

	// Resolve DSNs from the keychain so callers receive ready-to-use entries.
	// We don't fail here on individual keychain misses — a freshly-imported
	// entry on a machine whose keychain doesn't have the DSN is a legitimate
	// (recoverable) state. dbpool.Get surfaces the error at query time with
	// a clear message.
	for i := range r.Entries {
		if r.Entries[i].DSN != "" {
			continue
		}
		dsn, err := GetDSN(r.Entries[i].UUID)
		if err != nil {
			return nil, fmt.Errorf("resolve DSN for %q: %w", r.Entries[i].Name, err)
		}
		r.Entries[i].DSN = dsn
	}
	return &r, nil
}

// Save persists the registry. Creates parent dir with 0700, file with 0600.
func (r *Registry) Save() error {
	dir := path.Dir(files.LocalAgentConnectionsFilePath())
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	b, err := json.MarshalIndent(r, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(files.LocalAgentConnectionsFilePath(), b, 0o600)
}

// FindByUUID looks up an entry by uuid. The daemon calls this on every
// RunQuery to resolve the connection_uuid from a server-side request.
func (r *Registry) FindByUUID(u string) (Entry, bool) {
	for _, e := range r.Entries {
		if e.UUID == u {
			return e, true
		}
	}
	return Entry{}, false
}

// FindByName is convenience for the CLI's `remove` and `list` commands.
func (r *Registry) FindByName(name string) (Entry, bool) {
	for _, e := range r.Entries {
		if strings.EqualFold(e.Name, name) {
			return e, true
		}
	}
	return Entry{}, false
}

// Add appends a new entry and writes its DSN to the keychain. The on-disk
// registry file never sees the DSN. Returns an error if the name is already
// taken or the keychain write fails.
func (r *Registry) Add(e Entry) (Entry, error) {
	if strings.TrimSpace(e.Name) == "" {
		return Entry{}, errors.New("name is required")
	}
	if _, dup := r.FindByName(e.Name); dup {
		return Entry{}, fmt.Errorf("a connection named %q already exists", e.Name)
	}
	if e.UUID == "" {
		e.UUID = uuid.Must(uuid.NewV4()).String()
	}
	if e.DSN != "" {
		if err := PutDSN(e.UUID, e.DSN); err != nil {
			return Entry{}, fmt.Errorf("store DSN in keychain: %w", err)
		}
	}
	// Strip the legacy field defensively in case a caller populated it.
	e.LegacyDSN = ""
	r.Entries = append(r.Entries, e)
	return e, nil
}

// Remove deletes the entry matching the identifier (uuid or name) AND its
// keychain DSN. Idempotent on the keychain side (missing key isn't an error)
// so a partial-failure recovery can replay the call.
func (r *Registry) Remove(idOrName string) (Entry, error) {
	for i, e := range r.Entries {
		if e.UUID == idOrName || strings.EqualFold(e.Name, idOrName) {
			if err := DeleteDSN(e.UUID); err != nil {
				return Entry{}, fmt.Errorf("delete DSN from keychain: %w", err)
			}
			r.Entries = append(r.Entries[:i], r.Entries[i+1:]...)
			return e, nil
		}
	}
	return Entry{}, fmt.Errorf("no connection matching %q", idOrName)
}
