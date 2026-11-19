// Command genopenapi regenerates pkg/node/apiv1/openapi.json, the management
// API's OpenAPI document, from the API's route table and its wire types, so
// the document never drifts from the API.
//
// Usage:
//
//	go run ./cmd/genopenapi            # writes pkg/node/apiv1/openapi.json
//	go run ./cmd/genopenapi -out FILE  # writes to FILE
package main

import (
	"flag"
	"fmt"
	"os"

	"github.com/cubbit/dittofs/internal/node/api"
)

func main() {
	out := flag.String("out", "pkg/node/apiv1/openapi.json", "output file path")
	flag.Parse()
	b, err := api.OpenAPI()
	if err == nil {
		err = os.WriteFile(*out, b, 0o644)
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "genopenapi: %v\n", err)
		os.Exit(1)
	}
	fmt.Printf("wrote %s\n", *out)
}
