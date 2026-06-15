package main

import (
	"os"
	"strings"
	"testing"
)

func TestMCPToolExposesFullBarkPayloadFields(t *testing.T) {
	source, err := os.ReadFile("route_mcp.go")
	if err != nil {
		t.Fatal(err)
	}
	text := string(source)
	for _, field := range []string{"id", "autoCopy", "action", "ciphertext", "iv", "delete"} {
		if !strings.Contains(text, `mcp.WithString("`+field+`"`) {
			t.Fatalf("mcp notify tool should expose %s", field)
		}
	}
}
