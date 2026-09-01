// Package api embeds the service's machine-readable API contracts so the
// running binary can always serve the spec it was built from.
package api

import _ "embed"

// OpenAPISpec is the OpenAPI 3.1 description of the REST interface, served at
// GET /openapi.yaml.
//
//go:embed openapi.yaml
var OpenAPISpec []byte
