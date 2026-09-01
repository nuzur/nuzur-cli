package productclient

import (
	"context"
	"crypto/x509"
	"fmt"
	"os"
	"time"

	"github.com/nuzur/nuzur-cli/constants"
	"github.com/nuzur/nuzur-cli/files"
	pb "github.com/nuzur/nuzur-cli/protodeps/gen"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"
)

type Client struct {
	conn          *grpc.ClientConn
	ProductClient pb.NuzurProductClient
}

type Params struct {
	PRODUCT_API_ADDRESS *string
	DisableTLS          bool
}

func New(params Params) (*Client, error) {

	// build grpc client
	var opts []grpc.DialOption
	if params.DisableTLS {
		opts = append(opts, grpc.WithTransportCredentials(insecure.NewCredentials()))
	} else {
		pool, err := x509.SystemCertPool()
		if err != nil {
			return nil, err
		}
		creds := credentials.NewClientTLSFromCert(pool, "")
		opts = append(opts, grpc.WithTransportCredentials(creds))
	}

	api_address := constants.PRODUCT_PROD_ADDRESS
	if params.PRODUCT_API_ADDRESS != nil {
		api_address = *params.PRODUCT_API_ADDRESS
	}
	conn, err := grpc.NewClient(api_address, opts...)
	if err != nil {
		return nil, err
	}

	productClient := pb.NewNuzurProductClient(conn)

	return &Client{
		conn:          conn,
		ProductClient: productClient,
	}, nil
}

// defaultCallTimeout is the deadline every ordinary product RPC gets. It is
// sized for a control-plane request that carries no payload worth mentioning;
// anything that ships bytes (a file upload) must ask for its own budget with
// ClientContextWithTimeout instead.
const defaultCallTimeout = 10 * time.Second

func ClientContext() (context.Context, error) {
	// The cancel func is dropped deliberately: this signature has no way to
	// hand it back, and the deadline still releases the context's resources
	// when it fires. ClientContextWithTimeout is the variant to use when the
	// caller can honour a cancel.
	ctx, _, err := ClientContextWithTimeout(defaultCallTimeout)
	return ctx, err
}

// ClientContextWithTimeout builds the same authenticated context as
// ClientContext with a caller-chosen deadline, and returns the cancel func.
//
// It exists for calls whose duration depends on how much data they move rather
// than on how fast the server answers — uploading a file field's contents, say,
// where the default ten seconds is a size limit dressed up as a timeout.
//
// Callers must `defer cancel()`.
func ClientContextWithTimeout(timeout time.Duration) (context.Context, context.CancelFunc, error) {
	tokenBytes, err := os.ReadFile(files.TokenFilePath())
	if err != nil {
		return nil, nil, err
	}

	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	ctx = metadata.NewOutgoingContext(ctx, metadata.New(map[string]string{
		"authorization": fmt.Sprintf("bearer %s", string(tokenBytes)),
	}))
	return ctx, cancel, nil
}
