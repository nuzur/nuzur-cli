package fieldfile

import (
	"fmt"
	"strings"

	nemgen "github.com/nuzur/nem/idl/gen"
)

// UsesObjectStore reports whether the field stores its files in the team's own
// object store, as opposed to nuzur's internal bucket (BINARY).
func UsesObjectStore(cfg *nemgen.FieldTypeFileConfig) bool {
	return cfg.GetStorageType() == nemgen.FieldTypeFileConfigStorageType_FIELD_TYPE_FILE_CONFIG_STORAGE_TYPE_OBJECT_STORE
}

// ObjectKey reproduces the key the server writes an OBJECT_STORE upload to:
//
//	NormalizeKey(cfg.StorageConfig.ObjectStore.Path + "/" + fileName)
//
// It is for display only — the upload RPC returns a presigned URL and never the
// key, and this is the only way to tell the user where the bytes actually went.
//
// Note there is no project, entity or field component in the key. Two fields
// configured with the same path share a namespace, which is why an upload that
// would overwrite is refused unless --force is passed.
//
// Returns "" for a BINARY field, whose key is chosen by the server (it embeds a
// random uuid) and therefore cannot be predicted here.
func ObjectKey(cfg *nemgen.FieldTypeFileConfig, fileName string) string {
	if !UsesObjectStore(cfg) {
		return ""
	}
	return NormalizeKey(fmt.Sprintf("%s/%s", cfg.GetStorageConfig().GetObjectStore().GetPath(), fileName))
}

// RecordValue turns the presigned URL the upload returns into the value the
// data manager stores in the record column.
//
// The data manager persists `value.split('?')[0]` — the URL with the signature
// query stripped — and re-signs it on demand when rendering. So the signed URL
// the RPC hands back is NOT the thing to write into a record: it expires in 24
// hours. This is the distinction the CLI's output exists to make obvious.
func RecordValue(signedURL string) string {
	if q := strings.IndexByte(signedURL, '?'); q != -1 {
		return signedURL[:q]
	}
	return signedURL
}
