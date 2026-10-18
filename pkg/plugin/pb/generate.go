// Package pluginv1 holds the generated Go code for the Open Factory plugin
// protocol (proto/openfactory/plugin/v1). Do not edit the *.pb.go files;
// run `go generate ./pkg/plugin/pb` after changing the .proto files.
//
// The version 2 task backend service (proto/openfactory/plugin/v2) generates
// into the subdirectory v2 (package pluginv2).
//
// EXPERIMENTAL as a Go API: the wire protocol is versioned by
// the proto package (openfactory.plugin.v1), but the generated Go code may
// change when the code generators are upgraded.
package pluginv1

//go:generate protoc -I ../../../proto --go_out=../../.. --go_opt=module=github.com/openfactory-org/openfactory --go-grpc_out=../../.. --go-grpc_opt=module=github.com/openfactory-org/openfactory openfactory/plugin/v1/common.proto openfactory/plugin/v1/plugin.proto openfactory/plugin/v1/forge.proto openfactory/plugin/v1/responder.proto openfactory/plugin/v1/ingress.proto openfactory/plugin/v1/host.proto
//go:generate protoc -I ../../../proto --go_out=../../.. --go_opt=module=github.com/openfactory-org/openfactory --go-grpc_out=../../.. --go-grpc_opt=module=github.com/openfactory-org/openfactory openfactory/plugin/v2/sandbox.proto
