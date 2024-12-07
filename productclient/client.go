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
	API_ADDRESS *string
	DisableTLS  bool
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

	api_address := constants.API_PROD_ADDRESS
	if params.API_ADDRESS != nil {
		api_address = *params.API_ADDRESS
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

func ClientContext() (context.Context, error) {
	tokenBytes, err := os.ReadFile(files.TokenFilePath())
	if err != nil {
		return nil, err
	}
	ctx, _ := context.WithTimeout(context.Background(), 10*time.Second)
	ctx = metadata.NewOutgoingContext(ctx, metadata.New(map[string]string{
		"authorization": fmt.Sprintf("bearer %s", string(tokenBytes)),
	}))
	return ctx, nil
}
