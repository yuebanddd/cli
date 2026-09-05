package chatgptapp

import (
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"
)

func TestCreateVideoToolDeclaresChatGPTFileParam(t *testing.T) {
	tool := createVideoTool()
	if got, want := tool.Meta["openai/fileParams"], []string{"files"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("openai/fileParams = %#v, want %#v", got, want)
	}

	schema, ok := tool.InputSchema.(map[string]any)
	if !ok {
		t.Fatalf("InputSchema type = %T", tool.InputSchema)
	}
	defs := schema["$defs"].(map[string]any)
	fileSchema := defs["OpenAIFile"].(map[string]any)
	properties := fileSchema["properties"].(map[string]any)
	for _, name := range []string{"download_url", "file_id", "mime_type", "file_name"} {
		if _, ok := properties[name]; !ok {
			t.Fatalf("OpenAIFile schema is missing %q", name)
		}
	}
	if got, want := fileSchema["required"], []string{"download_url", "file_id"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("OpenAIFile required = %#v, want %#v", got, want)
	}
}

func TestBearerAuth(t *testing.T) {
	handler := bearerAuth("secret", http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		writer.WriteHeader(http.StatusNoContent)
	}))

	unauthorized := httptest.NewRecorder()
	handler.ServeHTTP(unauthorized, httptest.NewRequest(http.MethodPost, "/mcp", nil))
	if unauthorized.Code != http.StatusUnauthorized {
		t.Fatalf("unauthorized status = %d", unauthorized.Code)
	}

	authorizedRequest := httptest.NewRequest(http.MethodPost, "/mcp", nil)
	authorizedRequest.Header.Set("Authorization", "Bearer secret")
	authorized := httptest.NewRecorder()
	handler.ServeHTTP(authorized, authorizedRequest)
	if authorized.Code != http.StatusNoContent {
		t.Fatalf("authorized status = %d", authorized.Code)
	}
}

func TestNormalizeMCPPath(t *testing.T) {
	if got, err := normalizeMCPPath("/custom/"); err != nil || got != "/custom" {
		t.Fatalf("normalizeMCPPath() = %q, %v", got, err)
	}
	if _, err := normalizeMCPPath("mcp"); err == nil {
		t.Fatal("normalizeMCPPath() accepted a path without a leading slash")
	}
}
