package fieldfile

import (
	"fmt"
	"time"
)

// DefaultMaxBytes is the size at which the CLI refuses to upload without being
// told explicitly to (--max-bytes).
//
// It is deliberately far below what the server permits. product's gRPC server
// sets MaxRecvMsgSize to 1 GiB, but that is a permission, not a capacity: the
// upload is a UNARY call, so the file is held whole in the HTTP/2 frame buffer,
// again in the unmarshalled FileData, and again by the S3/R2 SDK — and the
// product deployment runs at replicaCount 1 with a 512Mi memory limit
// (.helm/product/values.yaml in nuzur-go). A big enough scripted upload can
// OOM the single replica that serves the entire control plane, including the
// web app, for every customer.
//
// The web never made large uploads convenient. A CLI does, which is exactly why
// the guard belongs here.
const DefaultMaxBytes int64 = 64 * 1024 * 1024

// MaxTimeout is the longest deadline worth asking for. The nginx ingress in
// front of product.nuzur.com sets proxy-read-timeout and proxy-send-timeout to
// 600 seconds, so a client deadline beyond that only changes which side reports
// the failure — and the ingress's version of it is the less informative one.
const MaxTimeout = 10 * time.Minute

// CheckSizeGuard applies the client-side ceiling. maxBytes <= 0 disables it.
func CheckSizeGuard(sizeBytes, maxBytes int64) error {
	if maxBytes <= 0 || sizeBytes <= maxBytes {
		return nil
	}
	return fmt.Errorf(
		"file is %s, above the %s client limit.\n"+
			"nuzur buffers an upload whole in the API pod, so a file this large can destabilize it for everyone.\n"+
			"Pass --max-bytes to override if you know this is safe, or write to the object store directly",
		HumanSize(sizeBytes), HumanSize(maxBytes))
}
