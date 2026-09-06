package mcpserver

import (
	"context"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/Pippit-dev/pippit-cli/internal/common"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func TestLocalToolCatalogStaysCompatible(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	server := newService(&common.Runner{}, DefaultOptions()).newProtocolServer(nil)
	serverTransport, clientTransport := mcp.NewInMemoryTransports()
	serverSession, err := server.Connect(ctx, serverTransport, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer serverSession.Close()
	client := mcp.NewClient(&mcp.Implementation{Name: "local-catalog-test", Version: "1"}, nil)
	session, err := client.Connect(ctx, clientTransport, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer session.Close()
	result, err := session.ListTools(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	want := make(map[string]bool, len(ToolNames))
	for _, name := range ToolNames {
		want[name] = true
	}
	for _, tool := range result.Tools {
		if !want[tool.Name] {
			t.Fatal("unexpected local tool", tool.Name)
		}
		delete(want, tool.Name)
	}
	if len(want) != 0 {
		t.Fatal("local tools removed", want)
	}
}

func TestFileInputMatchesOpenAIFileParameterShape(t *testing.T) {
	typeInfo := reflect.TypeOf(FileInput{})
	if typeInfo.NumField() != 4 {
		t.Fatalf("FileInput has %d fields; OpenAI file parameters require exactly four declared fields", typeInfo.NumField())
	}

	want := map[string]bool{
		"download_url": true,
		"file_id":      true,
		"mime_type":    false,
		"file_name":    false,
	}
	for index := 0; index < typeInfo.NumField(); index++ {
		field := typeInfo.Field(index)
		parts := strings.Split(field.Tag.Get("json"), ",")
		name := parts[0]
		required, ok := want[name]
		if !ok {
			t.Fatalf("FileInput contains unsupported JSON field %q", name)
		}
		optional := len(parts) > 1 && parts[1] == "omitempty"
		if required == optional {
			t.Fatalf("FileInput field %q required=%v, json tag=%q", name, required, field.Tag.Get("json"))
		}
		delete(want, name)
	}
	if len(want) != 0 {
		t.Fatalf("FileInput is missing fields: %#v", want)
	}
}

func TestAllFileToolsAdvertiseTopLevelFileParams(t *testing.T) {
	cases := []struct {
		name   string
		params []string
	}{
		{"upload", []string{"files"}},
		{"nest", []string{"files"}},
		{"image", []string{"images"}},
		{"video", []string{"images", "videos", "audios"}},
		{"short-drama", []string{"files"}},
		{"video-tool", []string{"videos"}},
		{"canvas", []string{"files"}},
	}
	for _, test := range cases {
		tool := toolDefinition("test_"+test.name, test.name, "test", false, false, false, true, test.params, "", "")
		got, ok := tool.Meta["openai/fileParams"].([]string)
		if !ok {
			t.Fatalf("%s tool did not advertise openai/fileParams as []string: %#v", test.name, tool.Meta)
		}
		if !reflect.DeepEqual(got, test.params) {
			t.Fatalf("%s file params = %#v, want %#v", test.name, got, test.params)
		}
	}
}
