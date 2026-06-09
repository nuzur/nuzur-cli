#! /bin/bash
go install google.golang.org/protobuf/cmd/protoc-gen-go@latest
go install google.golang.org/grpc/cmd/protoc-gen-go-grpc@latest
# https://grpc.io/docs/languages/go/quickstart/
export PATH="$PATH:$(go env GOPATH)/bin"

find ./gen -name "*.go" -type f -delete

# product proto
protoc --go_out=. --go-grpc_out=. --proto_path=./../../nuzur-go/product/idl/proto --proto_path=./../../nem/idl/proto ./../../nuzur-go/product/idl/proto/*.proto

# connection-manager proto (LocalAgentChannel for the agent daemon)
protoc --go_out=. --go-grpc_out=. --proto_path=./../../nuzur-go/connection-manager/idl/proto --proto_path=./../../nem/idl/proto ./../../nuzur-go/connection-manager/idl/proto/*.proto

sed -i '' 's/nem\/idl\/gen/github.com\/nuzur\/nem\/idl\/gen/g' ./gen/product.pb.go
sed -i '' 's/nem\/idl\/gen/github.com\/nuzur\/nem\/idl\/gen/g' ./gen/product_grpc.pb.go
sed -i '' 's/nem\/idl\/gen/github.com\/nuzur\/nem\/idl\/gen/g' ./gen/connection_manager.pb.go
sed -i '' 's/nem\/idl\/gen/github.com\/nuzur\/nem\/idl\/gen/g' ./gen/connection_manager_grpc.pb.go