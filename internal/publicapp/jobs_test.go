package publicapp

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Pippit-dev/pippit-cli/internal/auth"
	"github.com/Pippit-dev/pippit-cli/internal/common"
	"github.com/Pippit-dev/pippit-cli/internal/mcpserver"
)

func getJobThroughMCP(t *testing.T, a *App, token, key string) mcpserver.PublicJob {
	t.Helper()
	w := mcpRequest(a, token, "tools/call", map[string]any{"name": "pippit_get_job", "arguments": map[string]any{"idempotency_key": key}})
	var response struct {
		Result struct {
			IsError bool                `json:"isError"`
			Job     mcpserver.PublicJob `json:"structuredContent"`
		} `json:"result"`
	}
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &response) != nil || response.Result.IsError {
		t.Fatalf("job query: %d %s", w.Code, w.Body)
	}
	return response.Result.Job
}

func TestPostgresJobRecoveryUsesTenantAndOriginalKey(t *testing.T) {
	a := testApp(t)
	_, tokensA := testUser(t, a, "alice")
	_, tokensB := testUser(t, a, "bob")
	p := policyFor(t, a, tokensA.AccessToken)
	ctx := context.Background()
	var calls atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.WriteHeader(500)
	}))
	defer upstream.Close()
	a.clientFactory = func(auth common.RequestAuthorizer) common.Client {
		return common.NewHTTPClient(upstream.URL, time.Second, auth)
	}
	key := "lost-response-001"
	_, e := p.Execute(ctx, "pippit_generate_video", false, []byte(`{"idempotency_key":"lost-response-001","prompt":"private-prompt"}`), func(context.Context) ([]byte, error) {
		return []byte(`{"thread_id":"recovered-thread","run_id":"recovered-run"}`), nil
	})
	if e != nil {
		t.Fatal(e)
	}
	job := getJobThroughMCP(t, a, tokensA.AccessToken, key)
	if !job.Found || job.JobID == "" || job.ThreadID != "recovered-thread" || job.RunID != "recovered-run" || job.SubmissionState != "completed" || job.GenerationFinished || job.Submission == nil {
		t.Fatalf("lost submission not recovered: %+v", job)
	}
	encoded, _ := json.Marshal(job)
	for _, forbidden := range []string{"private-prompt", "key-for-alice", "account_id", "user_id", "request_hash"} {
		if strings.Contains(string(encoded), forbidden) {
			t.Fatal("job exposed internal data", forbidden)
		}
	}
	for _, unknown := range []string{key, "missing-key", "short", job.JobID} {
		if other := getJobThroughMCP(t, a, tokensB.AccessToken, unknown); other.Found || other.JobID != "" || other.Submission != nil || other.Result != nil {
			t.Fatal("foreign or unknown operation exposed", other)
		}
	}
	if _, e = p.Execute(ctx, "pippit_query_result", true, []byte(`{"thread_id":"recovered-thread","run_id":"recovered-run"}`), func(context.Context) ([]byte, error) {
		return []byte(`{"completed":true,"thread_id":"recovered-thread","run_id":"recovered-run","videos":[{"download_url":"https://example.test/result.mp4"}]}`), nil
	}); e != nil {
		t.Fatal(e)
	}
	job = getJobThroughMCP(t, a, tokensA.AccessToken, key)
	if !job.GenerationFinished || job.Result == nil {
		t.Fatal("completed generation metadata missing")
	}
	if calls.Load() != 0 {
		t.Fatal("metadata recovery called upstream")
	}
	largeMetadata, _ := json.Marshal(map[string]string{"text": strings.Repeat("x", 1100000)})
	if _, e = a.store.DB.Exec(`UPDATE jobs SET response=$1,result_metadata=$1`, largeMetadata); e != nil {
		t.Fatal(e)
	}
	job = getJobThroughMCP(t, a, tokensA.AccessToken, key)
	if !job.Found || !job.MetadataOmitted || job.Submission != nil || job.Result == nil || job.RunID != "recovered-run" {
		t.Fatal("large metadata prevented bounded recovery")
	}
	if _, e = a.store.DB.Exec(`UPDATE jobs SET updated_at=now()-interval '31 days'`); e != nil {
		t.Fatal(e)
	}
	if e = a.store.Cleanup(ctx); e != nil {
		t.Fatal(e)
	}
	job = getJobThroughMCP(t, a, tokensA.AccessToken, key)
	if !job.Found || job.Submission != nil || job.Result != nil || job.ThreadID != "recovered-thread" || !job.GenerationFinished {
		t.Fatal("tombstone did not preserve safe recovery identifiers", job)
	}
	tx, e := a.store.DB.BeginTx(ctx, nil)
	if e != nil {
		t.Fatal(e)
	}
	defer tx.Rollback()
	if _, e = tx.ExecContext(ctx, `UPDATE xyq_accounts SET status='disconnected' WHERE user_id=$1`, p.principal.UserID); e != nil {
		t.Fatal(e)
	}
	device, e := auth.NewRemoteDeviceID()
	if e != nil {
		t.Fatal(e)
	}
	if e = a.store.saveCredential(ctx, tx, p.principal.UserID, &auth.Credential{UID: "replacement-account", DeviceID: device, TokenID: "replacement-token", AccessKey: "replacement-key", ExpiredAt: time.Now().Add(time.Hour).Unix()}); e != nil {
		t.Fatal(e)
	}
	if e = tx.Commit(); e != nil {
		t.Fatal(e)
	}
	if job = getJobThroughMCP(t, a, tokensA.AccessToken, key); job.Found {
		t.Fatal("replacement account read previous account's job")
	}
}

func TestPostgresPendingJobReadableWithoutResubmission(t *testing.T) {
	a := testApp(t)
	_, tokens := testUser(t, a, "alice")
	p := policyFor(t, a, tokens.AccessToken)
	entered, release := make(chan struct{}), make(chan struct{})
	done := make(chan error, 1)
	go func() {
		_, err := p.Execute(context.Background(), "pippit_generate_video", false, []byte(`{"idempotency_key":"pending-job-001"}`), func(context.Context) ([]byte, error) {
			close(entered)
			<-release
			return nil, errors.New("connection lost after submission")
		})
		done <- err
	}()
	defer func() { close(release); <-done }()
	select {
	case <-entered:
	case err := <-done:
		// Leave the cleanup receive satisfied when execution failed early.
		done <- err
		t.Fatal("submission failed before execution", err)
	case <-time.After(5 * time.Second):
		t.Fatal("submission did not start")
	}
	job := getJobThroughMCP(t, a, tokens.AccessToken, "pending-job-001")
	if !job.Found || job.SubmissionState != "pending" || job.ThreadID != "" || job.GenerationFinished {
		t.Fatal("pending operation not readable", job)
	}
}

func TestPostgresPostExecutionFailuresBecomeUncertain(t *testing.T) {
	for _, failure := range []string{"invalid-json", "oversized", "ownership", "persistence", "cancelled"} {
		t.Run(failure, func(t *testing.T) {
			a := testApp(t)
			_, tokens := testUser(t, a, "alice")
			p := policyFor(t, a, tokens.AccessToken)
			if failure == "persistence" {
				_, e := a.store.DB.Exec(`CREATE FUNCTION reject_completed_job() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN IF NEW.state='completed' THEN RAISE EXCEPTION 'fixture persistence failure'; END IF; RETURN NEW; END $$;
CREATE TRIGGER reject_completed_job BEFORE UPDATE ON jobs FOR EACH ROW EXECUTE FUNCTION reject_completed_job()`)
				if e != nil {
					t.Fatal(e)
				}
			}
			if failure == "ownership" {
				_, other := testUser(t, a, "bob")
				b := policyFor(t, a, other.AccessToken)
				_, e := b.Execute(context.Background(), "pippit_generate_video", false, []byte(`{"idempotency_key":"bob-original"}`), func(context.Context) ([]byte, error) {
					return []byte(`{"thread_id":"foreign","run_id":"foreign-run"}`), nil
				})
				if e != nil {
					t.Fatal(e)
				}
			}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			var calls int
			_, e := p.Execute(ctx, "pippit_generate_video", false, []byte(`{"idempotency_key":"failed-response"}`), func(context.Context) ([]byte, error) {
				calls++
				switch failure {
				case "invalid-json":
					return []byte(`not-json`), nil
				case "oversized":
					return []byte(strings.Repeat("x", (2<<20)+1)), nil
				case "ownership":
					return []byte(`{"thread_id":"foreign","run_id":"foreign-run"}`), nil
				case "cancelled":
					cancel()
					return nil, context.Canceled
				default:
					return []byte(`{"thread_id":"new-thread","run_id":"new-run"}`), nil
				}
			})
			if e == nil {
				t.Fatal("failure expected")
			}
			job := getJobThroughMCP(t, a, tokens.AccessToken, "failed-response")
			if !job.Found || job.SubmissionState != "uncertain" || job.Submission != nil {
				t.Fatal("failure not recorded immediately", job)
			}
			var count int
			if e = a.store.DB.QueryRow(`SELECT count(*) FROM audit_events WHERE user_id=$1 AND event='pippit_generate_video' AND outcome='failed' AND error_class='operation_error'`, p.principal.UserID).Scan(&count); e != nil || count != 1 {
				t.Fatal("missing terminal failure audit", count, e)
			}
			_, e = p.Execute(context.Background(), "pippit_generate_video", false, []byte(`{"idempotency_key":"failed-response"}`), func(context.Context) ([]byte, error) { calls++; return []byte(`{}`), nil })
			if e == nil || calls != 1 {
				t.Fatal("uncertain request was repeated")
			}
			if e = a.store.DB.QueryRow(`SELECT count(*) FROM audit_events WHERE user_id=$1 AND event='pippit_generate_video' AND outcome='retry_blocked'`, p.principal.UserID).Scan(&count); e != nil || count != 1 {
				t.Fatal("blocked retry was not audited", count, e)
			}
		})
	}
}
