package publicapp

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/Pippit-dev/pippit-cli/internal/auth"
	"github.com/Pippit-dev/pippit-cli/internal/config"
)

type identityTransport func(*http.Request) (*http.Response, error)

func (f identityTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestXiaoyunqueIdentityRejectsUnverifiedSubjects(t *testing.T) {
	for _, tc := range []struct {
		name, body string
		status     int
		valid      bool
	}{
		{"verified", `{"ret":"0","data":{"uid":"alice"}}`, 200, true},
		{"different owner", `{"ret":"0","data":{"uid":"bob"}}`, 200, false},
		{"missing owner", `{"ret":"0","data":{}}`, 200, false},
		{"business error", `{"ret":"403","data":{"uid":"alice"}}`, 200, false},
		{"http error", `{"ret":"0","data":{"uid":"alice"}}`, 403, false},
		{"login page", `<html>sign in</html>`, 200, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			v := &xiaoyunqueAuthorization{client: &http.Client{Transport: identityTransport(func(r *http.Request) (*http.Response, error) {
				if r.URL.String() != "https://xyq.jianying.com/api/biz/v1/user/info" || r.Method != "GET" || r.Header.Get("Authorization") != "Bearer fixture" || r.Header.Get("Cookie") != "" {
					t.Fatal("identity request changed")
				}
				return &http.Response{StatusCode: tc.status, Body: io.NopCloser(strings.NewReader(tc.body))}, nil
			})}}
			_, e := v.Verify(context.Background(), auth.RemoteAccessKeyPayload{UID: "alice", AccessKey: "fixture"})
			if (e == nil) != tc.valid {
				t.Fatal("unexpected identity decision", e)
			}
		})
	}
}

// Explicit live release gate. Reads an existing local browser credential only
// for this test; it never binds it to a public user or logs identity/secrets.
func TestLiveXiaoyunqueIdentity(t *testing.T) {
	if os.Getenv("PIPPIT_TEST_LIVE_XYQ_IDENTITY") != "1" {
		t.Skip("set PIPPIT_TEST_LIVE_XYQ_IDENTITY=1 to verify an existing browser login against production")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	manager := auth.NewManager(&config.Config{})
	status, e := manager.Status(ctx)
	if e != nil || !status.LoggedIn || status.UID == "" {
		t.Fatal("a valid local browser login is required for this read-only release gate")
	}
	key, e := manager.ResolveAccessKey(ctx)
	if e != nil {
		t.Fatal("local browser credential could not be resolved")
	}
	verifier := NewXiaoyunqueAuthorization().(*xiaoyunqueAuthorization)
	verifier.client.Transport = identityTransport(func(r *http.Request) (*http.Response, error) {
		response, err := http.DefaultTransport.RoundTrip(r)
		if err == nil {
			t.Logf("identity endpoint HTTP status: %d", response.StatusCode)
			body, readErr := io.ReadAll(io.LimitReader(response.Body, 64<<10))
			response.Body.Close()
			response.Body = io.NopCloser(bytes.NewReader(body))
			var diagnostic struct {
				Ret  json.Number `json:"ret"`
				Data struct {
					UID string `json:"uid"`
				} `json:"data"`
			}
			decodeErr := json.Unmarshal(body, &diagnostic)
			if code, parseErr := strconv.ParseInt(string(diagnostic.Ret), 10, 64); parseErr == nil {
				t.Logf("identity business code: %d; subject present: %v; subject matches: %v", code, diagnostic.Data.UID != "", diagnostic.Data.UID == status.UID)
			}
			t.Logf("identity response parsed: %v", readErr == nil && decodeErr == nil)
		}
		return response, err
	})
	_, e = verifier.Verify(ctx, auth.RemoteAccessKeyPayload{UID: status.UID, AccessKey: key, ExpiredAt: status.ExpiresAt.Unix()})
	if e != nil {
		t.Fatal("production identity verification failed; Public binding remains unverified:", e)
	}
}
