// Keyring wraps OS-native secret storage for connection DSNs. The agent's
// per-connection registry persists metadata (uuid, name, driver, db_type,
// default_schema) in a JSON file, but the DSN — which carries the user's
// database password — lives here.
//
// Backend selection:
//   - macOS:   the user's login Keychain when available (cgo builds), falling
//              back to a passphrase-encrypted file under the agent's config
//              directory — the Keychain backend is gated on `darwin && cgo`,
//              so a CGO_ENABLED=0 binary would otherwise have no backend.
//   - Linux:   Secret Service (gnome-keyring / kwallet) when available,
//              falling back to a passphrase-encrypted file under the agent's
//              config directory for headless servers.
//   - Other:   pass-through file backend. We don't ship for Windows yet.
//
// Secret IDs are the connection's uuid; the service name is fixed
// ("nuzur-cli") so the user sees a single grouped entry per connection in
// their keychain UI.
package connections

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sync"

	"github.com/99designs/keyring"
	"github.com/nuzur/nuzur-cli/files"
)

const (
	keyringService = "nuzur-cli"
	// dsnSecretPrefix is prefixed on every key written to the keychain so a
	// human inspecting the keychain entry can tell what nuzur stores there.
	dsnSecretPrefix = "dsn-"
)

var (
	keyringOnce sync.Once
	keyringRef  keyring.Keyring
	keyringErr  error
)

// openKeyring lazily opens the OS keyring. Errors here are fatal for any
// connection that depends on stored secrets — the daemon can't open a DB if
// it can't read the DSN.
func openKeyring() (keyring.Keyring, error) {
	keyringOnce.Do(func() {
		cfg := keyring.Config{
			ServiceName: keyringService,
			// macOS — name the keychain item so it's recognizable in
			// Keychain Access.app.
			KeychainName:                 "login",
			KeychainTrustApplication:     true,
			KeychainAccessibleWhenUnlocked: true,
			KeychainSynchronizable:       false,
			// Linux Secret Service — collection label.
			LibSecretCollectionName: "login",
			// KWallet folder.
			KWalletAppID:  keyringService,
			KWalletFolder: keyringService,
			// pass — for nerds who use it. Skip configuring; users opting
			// into pass set $PASSWORD_STORE_DIR themselves.
			//
			// File backend — fallback for headless Linux. We seed the
			// passphrase from a stable machine-derived secret (below) so
			// the daemon can read its own store on restart without
			// prompting. This is best-effort encryption-at-rest, not a
			// defense against a local attacker with file-read access.
			FileDir:          fileBackendDir(),
			FilePasswordFunc: fileBackendPassphrase,
			AllowedBackends:  allowedBackends(),
		}
		keyringRef, keyringErr = keyring.Open(cfg)
	})
	return keyringRef, keyringErr
}

// allowedBackends keeps backend selection explicit per-OS. The "no Windows
// install path yet" decision excludes only the launchd/systemd equivalent
// (Windows SCM); secret storage via wincred works fine and is shipped here.
func allowedBackends() []keyring.BackendType {
	switch runtime.GOOS {
	case "darwin":
		// Keychain is preferred, but it's only compiled in for cgo builds
		// (the backend is gated on `darwin && cgo`). A CGO_ENABLED=0 binary —
		// or a headless/SSH session with no Keychain access — falls back to
		// the passphrase-encrypted file backend so secret storage still works.
		return []keyring.BackendType{
			keyring.KeychainBackend,
			keyring.FileBackend,
		}
	case "linux":
		return []keyring.BackendType{
			keyring.SecretServiceBackend,
			keyring.KWalletBackend,
			keyring.FileBackend,
		}
	case "windows":
		return []keyring.BackendType{keyring.WinCredBackend}
	default:
		return []keyring.BackendType{keyring.FileBackend}
	}
}

func fileBackendDir() string {
	// Sit alongside the registry file rather than a new top-level dir; reuses
	// the same 0700 parent we already create on Save().
	return filepath.Join(filepath.Dir(files.LocalAgentConnectionsFilePath()), "keyring")
}

// fileBackendPassphrase returns a deterministic passphrase derived from
// machine identifiers so the daemon can decrypt its own store unattended
// after a reboot. It is NOT a cryptographic secret — anyone with code
// execution as the same user can derive it. The real protection on the
// file backend is the 0700 parent dir + 0600 file perms.
func fileBackendPassphrase(_ string) (string, error) {
	h := sha256.New()
	if hn, err := os.Hostname(); err == nil {
		_, _ = h.Write([]byte(hn))
	}
	// /etc/machine-id is stable across reboots on systemd Linux. macOS doesn't
	// have it; on macOS the file backend isn't reached (Keychain is allowed).
	if b, err := os.ReadFile("/etc/machine-id"); err == nil {
		_, _ = h.Write(b)
	}
	// Fall back to user uid so two users on the same machine get distinct
	// passphrases even on hosts without machine-id.
	if u := os.Getenv("USER"); u != "" {
		_, _ = h.Write([]byte(u))
	}
	_, _ = h.Write([]byte("nuzur-cli-keyring-v1"))
	return hex.EncodeToString(h.Sum(nil)), nil
}

func dsnKey(connUUID string) string {
	return dsnSecretPrefix + connUUID
}

// PutDSN writes a connection's DSN to the keychain. Overwrites any existing
// secret with the same uuid.
func PutDSN(connUUID, dsn string) error {
	kr, err := openKeyring()
	if err != nil {
		return fmt.Errorf("open keyring: %w", err)
	}
	return kr.Set(keyring.Item{
		Key:         dsnKey(connUUID),
		Data:        []byte(dsn),
		Label:       fmt.Sprintf("nuzur connection %s", connUUID),
		Description: "DSN for a nuzur agent local database connection",
	})
}

// GetDSN reads a connection's DSN. Returns ("", nil) when no secret exists
// for that uuid so callers can treat "missing" as a soft state rather than
// an error during legacy migration.
func GetDSN(connUUID string) (string, error) {
	kr, err := openKeyring()
	if err != nil {
		return "", fmt.Errorf("open keyring: %w", err)
	}
	item, err := kr.Get(dsnKey(connUUID))
	if err != nil {
		if errors.Is(err, keyring.ErrKeyNotFound) {
			return "", nil
		}
		return "", fmt.Errorf("read keyring: %w", err)
	}
	return string(item.Data), nil
}

// DeleteDSN removes a connection's DSN from the keychain. Idempotent in
// the not-found case so `agent connection remove` can be safely re-run.
func DeleteDSN(connUUID string) error {
	kr, err := openKeyring()
	if err != nil {
		return fmt.Errorf("open keyring: %w", err)
	}
	if err := kr.Remove(dsnKey(connUUID)); err != nil && !errors.Is(err, keyring.ErrKeyNotFound) {
		return fmt.Errorf("delete keyring: %w", err)
	}
	return nil
}
