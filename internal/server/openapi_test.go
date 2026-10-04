package server

import (
	"bytes"
	"flag"
	"os"
	"testing"
)

var update = flag.Bool("update", false, "rewrite docs/openapi.yaml")

const openAPIFile = "../../docs/openapi.yaml"

// The committed OpenAPI document must match the code. Regenerate it with
// go test ./internal/server -run TestOpenAPIFileIsCurrent -update
func TestOpenAPIFileIsCurrent(t *testing.T) {
	doc, err := OpenAPIYAML()
	if err != nil {
		t.Fatal(err)
	}
	if *update {
		if err := os.WriteFile(openAPIFile, doc, 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}
	committed, err := os.ReadFile(openAPIFile)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(committed, doc) {
		t.Fatalf("%s is out of date; run: go test ./internal/server -run TestOpenAPIFileIsCurrent -update", openAPIFile)
	}
}
