package cmd

import (
	"bytes"
	"testing"
)

func TestRootRegistersMCPServe(t *testing.T) {
	var stdout, stderr bytes.Buffer
	root := NewRootCommand(&stdout, &stderr)
	command, remaining, err := root.Find([]string{"mcp", "serve"})
	if err != nil {
		t.Fatalf("root.Find(mcp serve) error = %v", err)
	}
	if command == nil || command.Name() != "serve" {
		t.Fatalf("root.Find(mcp serve) command = %#v", command)
	}
	if len(remaining) != 0 {
		t.Fatalf("root.Find(mcp serve) remaining = %#v", remaining)
	}
}
