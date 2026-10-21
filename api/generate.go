package api

import _ "embed"

//go:generate go tool oapi-codegen --config oapi-codegen.yaml openapi.yaml

//go:embed openapi.yaml
var Specification []byte
