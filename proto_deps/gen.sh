#! /bin/bash
go install google.golang.org/protobuf/cmd/protoc-gen-go@latest
go install google.golang.org/grpc/cmd/protoc-gen-go-grpc@latest
# https://grpc.io/docs/languages/go/quickstart/
export PATH="$PATH:$(go env GOPATH)/bin"

find ./gen -name "*.go" -type f -delete
find ./nem/idl/gen -name "*.go" -type f -delete

protoc --go_out=. --go-grpc_out=. --proto_path=./../../nuzur-go/nem/idl/proto ./../../nuzur-go/nem/idl/proto/*.proto
protoc --go_out=. --go-grpc_out=. --proto_path=./../../nuzur-go/product/idl/proto --proto_path=./../../nuzur-go/nem/idl/proto ./../../nuzur-go/product/idl/proto/*.proto


sed -i '' 's/nem\/idl\/gen/github.com\/nuzur\/nuzur-cli\/proto_deps\/nem\/idl\/gen/g' ./gen/product.pb.go
sed -i '' 's/nem\/idl\/gen/github.com\/nuzur\/nuzur-cli\/proto_deps\/nem\/idl\/gen/g' ./gen/product_grpc.pb.go