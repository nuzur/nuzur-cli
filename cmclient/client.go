package cmclient

import (
	"crypto/x509"
	"os"

	"github.com/nuzur/nuzur-cli/constants"
	pb "github.com/nuzur/nuzur-cli/protodeps/gen"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/credentials/insecure"
)

// Client is the CLI-side gRPC client for the connection-manager service.
// Used by the agent daemon to dial LocalAgentChannel.
type Client struct {
	conn    *grpc.ClientConn
	CM      pb.NuzurConnectionManagerClient
	Address string // the resolved target, for diagnostics
}

type Params struct {
	// Optional override; if nil, falls back to env var NUZUR_CONNECTION_MANAGER_ADDRESS
	// or the prod default.
	Address    *string
	DisableTLS bool
}

func New(params Params) (*Client, error) {
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

	address := constants.CONNECTION_MANAGER_PROD_ADDRESS
	if params.Address != nil && *params.Address != "" {
		address = *params.Address
	} else if env := os.Getenv("NUZUR_CONNECTION_MANAGER_ADDRESS"); env != "" {
		address = env
	}

	conn, err := grpc.NewClient(address, opts...)
	if err != nil {
		return nil, err
	}
	return &Client{
		conn:    conn,
		CM:      pb.NewNuzurConnectionManagerClient(conn),
		Address: address,
	}, nil
}

func (c *Client) Close() error {
	if c.conn == nil {
		return nil
	}
	return c.conn.Close()
}
