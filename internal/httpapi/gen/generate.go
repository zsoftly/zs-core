// Package gen holds the Go types generated from api/openapi.yaml. Do not edit
// types.gen.go; change the OpenAPI document and run make generate.
package gen

//go:generate go tool oapi-codegen -config ../../../api/oapi-codegen.yaml -o types.gen.go ../../../api/openapi.yaml
