package chatgptapp

import (
	"context"
	"encoding/json"
	"reflect"
	"testing"

	"github.com/Pippit-dev/pippit-cli/internal/common"
	"github.com/Pippit-dev/pippit-cli/internal/config"
)

type fakeClient struct {
	path string
	body any
	json string
}

func (client *fakeClient) SendRequest(_ context.Context, path string, body any, out any) error {
	client.path = path
	client.body = body
	if client.json == "" || out == nil {
		return nil
	}
	return json.Unmarshal([]byte(client.json), out)
}

func (client *fakeClient) SendRequestWithHeaders(ctx context.Context, path string, body any, _ map[string]string, out any) error {
	return client.SendRequest(ctx, path, body, out)
}

func (client *fakeClient) SendMultipartRequest(_ context.Context, _ string, _ map[string]string, _ common.MultipartFile, _ any) error {
	return nil
}

func TestCreateVideoUsesExistingSubmitRunClient(t *testing.T) {
	client := &fakeClient{json: `{
		"ret":"0",
		"errmsg":"",
		"data":{
			"web_thread_link":"https://example.test/thread",
			"run":{"thread_id":"thread-1","run_id":"run-1"}
		}
	}`}
	service := NewService(&common.Runner{
		Config: &config.Config{Paths: &config.Paths{SubmitRun: "/submit"}},
		Client: client,
	})

	result, err := service.CreateVideo(context.Background(), CreateVideoInput{Prompt: "生成一条剧情广告"})
	if err != nil {
		t.Fatalf("CreateVideo() error = %v", err)
	}
	if client.path != "/submit" {
		t.Fatalf("request path = %q, want /submit", client.path)
	}
	if got, want := client.body, map[string]any{"message": "生成一条剧情广告"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("request body = %#v, want %#v", got, want)
	}
	if result.ThreadID != "thread-1" || result.RunID != "run-1" || result.WebThreadLink == "" {
		t.Fatalf("result = %#v", result)
	}
	if result.UploadedAssetIDs == nil || len(result.UploadedAssetIDs) != 0 {
		t.Fatalf("UploadedAssetIDs = %#v, want empty non-nil slice", result.UploadedAssetIDs)
	}
}

func TestContinueVideoKeepsThreadID(t *testing.T) {
	client := &fakeClient{json: `{
		"ret":"0",
		"data":{"run":{"thread_id":"thread-1","run_id":"run-2"}}
	}`}
	service := NewService(&common.Runner{
		Config: &config.Config{Paths: &config.Paths{SubmitRun: "/submit"}},
		Client: client,
	})

	_, err := service.ContinueVideo(context.Background(), ContinueVideoInput{
		ThreadID: "thread-1",
		Prompt:   "把镜头推进得更慢",
	})
	if err != nil {
		t.Fatalf("ContinueVideo() error = %v", err)
	}
	want := map[string]any{
		"message":   "把镜头推进得更慢",
		"thread_id": "thread-1",
	}
	if !reflect.DeepEqual(client.body, want) {
		t.Fatalf("request body = %#v, want %#v", client.body, want)
	}
}

func TestGetVideoStatusUsesReadableV2Response(t *testing.T) {
	client := &fakeClient{json: `{
		"ret":"0",
		"data":{"readable_text":"视频正在生成中"}
	}`}
	service := NewService(&common.Runner{
		Config: &config.Config{Paths: &config.Paths{GetThread: "/get-thread"}},
		Client: client,
	})

	result, err := service.GetVideoStatus(context.Background(), GetVideoStatusInput{
		ThreadID: "thread-1",
		RunID:    "run-1",
	})
	if err != nil {
		t.Fatalf("GetVideoStatus() error = %v", err)
	}
	if client.path != "/get-thread" {
		t.Fatalf("request path = %q, want /get-thread", client.path)
	}
	if result.ReadableText != "视频正在生成中" {
		t.Fatalf("ReadableText = %q", result.ReadableText)
	}
}
