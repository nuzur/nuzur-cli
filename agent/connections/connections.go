// Package connections holds the agent's local registry of database
// connections — the set of named (name, driver, DSN, db_type) entries the user
// has added via `nuzur agent connection add`.
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

// Entry is a single registered connection.
type Entry struct {
	UUID          string `json:"uuid"`
	Name          string `json:"name"`
	Driver        string `json:"driver"`         // mysql | postgres (matches sql.Open driver name)
	DBType        DBType `json:"db_type"`        // mirrors nem.LocalAgentConnectionDbType
	DSN           string `json:"dsn"`            // local-only, never published
	DefaultSchema string `json:"default_schema"` // optional
}

// Registry is the in-memory snapshot of all locally-registered connections.
type Registry struct {
	Entries []Entry `json:"entries"`
}

// Load reads the on-disk registry. Returns an empty Registry if the file
// doesn't exist yet (first run).
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

// Add appends a new entry. Returns an error if the name is already taken.
// UUID is generated if blank.
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
	r.Entries = append(r.Entries, e)
	return e, nil
}

// Remove deletes the entry matching the identifier (uuid or name). Returns
// the removed entry. Idempotent in the failure case (no match → not-found error).
func (r *Registry) Remove(idOrName string) (Entry, error) {
	for i, e := range r.Entries {
		if e.UUID == idOrName || strings.EqualFold(e.Name, idOrName) {
			r.Entries = append(r.Entries[:i], r.Entries[i+1:]...)
			return e, nil
		}
	}
	return Entry{}, fmt.Errorf("no connection matching %q", idOrName)
}
