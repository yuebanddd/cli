package publicapp

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"time"

	"github.com/Pippit-dev/pippit-cli/internal/auth"
)

// XiaoyunqueAuthorization verifies the ACCOUNT OWNER of an Access Key with a
// trusted upstream, independently of the browser-supplied UID/Origin/binding.
// The CLI callback protocol is not OAuth and does not itself attest identity.
type XiaoyunqueAuthorization interface {
	Verify(context.Context, auth.RemoteAccessKeyPayload) (string, error)
}

// RemoteIdentityVerifier is an integration boundary for an officially approved
// upstream identity/introspection adapter. It is deliberately not an invented
// Xiaoyunque endpoint. Configure only an operator-controlled HTTPS adapter that
// verifies the Access Key against Xiaoyunque (never merely echoes callback UID).
type RemoteIdentityVerifier struct {
	Endpoint string
	Client   *http.Client
}

func NewRemoteIdentityVerifier(endpoint string) (*RemoteIdentityVerifier, error) {
	u, e := url.Parse(endpoint)
	if e != nil || u.Scheme != "https" || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return nil, errors.New("a trusted HTTPS upstream identity verifier is required")
	}
	return &RemoteIdentityVerifier{endpoint, &http.Client{Timeout: 10 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return errors.New("identity verifier redirects forbidden") }}}, nil
}
func (v *RemoteIdentityVerifier) Verify(ctx context.Context, p auth.RemoteAccessKeyPayload) (string, error) {
	body, _ := json.Marshal(map[string]string{"access_key": p.AccessKey})
	r, e := http.NewRequestWithContext(ctx, "POST", v.Endpoint, bytes.NewReader(body))
	if e != nil {
		return "", errors.New("upstream_identity_unavailable")
	}
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("Accept", "application/json")
	resp, e := v.Client.Do(r)
	if e != nil {
		return "", errors.New("upstream_identity_unavailable")
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return "", errors.New("upstream_identity_rejected")
	}
	var out struct {
		Active    bool   `json:"active"`
		Subject   string `json:"subject"`
		ExpiresAt int64  `json:"expires_at"`
	}
	if json.NewDecoder(io.LimitReader(resp.Body, 8192)).Decode(&out) != nil || !out.Active || out.Subject == "" || len(out.Subject) > 256 || out.Subject != p.UID || out.ExpiresAt <= time.Now().Unix() || p.ExpiredAt > out.ExpiresAt {
		return "", errors.New("upstream_identity_rejected")
	}
	return out.Subject, nil
}

// NewXiaoyunqueAuthorization checks the authenticated user-info endpoint used
// by the Xiaoyunque web client. Public callback approval and this endpoint's AK
// authentication support remain live release gates; failures never trust UID.
func NewXiaoyunqueAuthorization() XiaoyunqueAuthorization {
	return &xiaoyunqueAuthorization{client: &http.Client{Timeout: 10 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return errors.New("identity redirects forbidden") }}}
}

type xiaoyunqueAuthorization struct{ client *http.Client }

func (v *xiaoyunqueAuthorization) Verify(ctx context.Context, p auth.RemoteAccessKeyPayload) (string, error) {
	r, e := http.NewRequestWithContext(ctx, "GET", "https://xyq.jianying.com/api/biz/v1/user/info", nil)
	if e != nil {
		return "", errors.New("upstream_identity_unavailable")
	}
	r.Header.Set("Authorization", "Bearer "+p.AccessKey)
	r.Header.Set("Accept", "application/json")
	resp, e := v.client.Do(r)
	if e != nil {
		return "", errors.New("upstream_identity_unavailable")
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return "", errors.New("upstream_identity_rejected")
	}
	var out struct {
		Ret  string `json:"ret"`
		Data struct {
			UID string `json:"uid"`
		} `json:"data"`
	}
	if json.NewDecoder(io.LimitReader(resp.Body, 64<<10)).Decode(&out) != nil || out.Ret != "0" || out.Data.UID == "" || out.Data.UID != p.UID {
		return "", errors.New("upstream_identity_rejected")
	}
	return out.Data.UID, nil
}
