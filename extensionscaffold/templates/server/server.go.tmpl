package server

import (
	"context"

	"github.com/nuzur/extension-sdk/client"
	pb "github.com/nuzur/extension-sdk/idl/gen"
	"go.uber.org/fx"
)

type server struct {
	pb.UnimplementedNuzurExtensionServer
	client   *client.Client
	metadata *pb.GetMetadataResponse
}

type Params struct {
	fx.In
	Client *client.Client
}

func New(params Params) (pb.NuzurExtensionServer, error) {
	metadata, err := params.Client.GetMetadata(context.Background(), &pb.GetMetadataRequest{})
	if err != nil {
		return nil, err
	}
	return &server{
		client:   params.Client,
		metadata: metadata,
	}, nil
}
