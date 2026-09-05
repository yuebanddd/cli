package auth

import (
	"crypto/rand"
	"errors"
	"fmt"
	"net/url"
	"strings"

	"github.com/Pippit-dev/pippit-cli/internal/config"
)

// RemoteLoginSource is the source identifier understood by the Xiaoyunque
// browser login page. Keep this aligned with the native CLI browser flow.
const RemoteLoginSource = loginSource

// RemoteAccessKeyPayload is posted by the Xiaoyunque browser login page to a
// caller-provided HTTPS callback after the user completes login/authorization.
// AccessKey is secret and must never be logged.
type RemoteAccessKeyPayload struct {
	Type            string `json:"type"`
	AccessKey       string `json:"access_key"`
	UID             string `json:"uid"`
	TokenID         string `json:"token_id"`
	ExpiredAt       int64  `json:"expired_at"`
	RandomSecretKey string `json:"random_secret_key"`
	Source          string `json:"source"`
	CallbackURL     string `json:"callback_url"`
}

// NewRemoteDeviceID creates a device-scoped identifier compatible with the
// Xiaoyunque CLI browser authorization page.
func NewRemoteDeviceID() (string, error) {
	return randomEncoded(rand.Reader, deviceIDBytes)
}

// NewRemoteBindingSecret creates the per-flow secret echoed by the Xiaoyunque
// browser page in its callback payload.
func NewRemoteBindingSecret() (string, error) {
	return randomEncoded(rand.Reader, randomBindingBytes)
}

// AccountBindingForUID returns the non-secret account binding used when a
// device needs to rotate/reuse a previously issued Xiaoyunque Access Key.
func AccountBindingForUID(uid string) string {
	return accountBinding(uid)
}

// BuildRemoteLoginURL builds a Xiaoyunque browser login URL whose credential
// callback targets a remote HTTPS MCP/OAuth service instead of a loopback CLI
// listener.
func BuildRemoteLoginURL(callbackURL, deviceID, tokenID, expectedAccount, randomSecret string, forceRefresh bool) (string, error) {
	callback, err := url.Parse(strings.TrimSpace(callbackURL))
	if err != nil {
		return "", fmt.Errorf("invalid remote login callback URL: %w", err)
	}
	if callback.Scheme != "https" || callback.Host == "" || callback.User != nil {
		return "", errors.New("remote login callback URL must be an absolute HTTPS URL without user info")
	}
	if !validDeviceID(deviceID) {
		return "", errors.New("remote login device ID is invalid")
	}
	if tokenID != "" && !validTokenID(tokenID) {
		return "", errors.New("remote login token ID is invalid")
	}
	if expectedAccount != "" && !validAccountBinding(expectedAccount) {
		return "", errors.New("remote login account binding is invalid")
	}
	if forceRefresh && (tokenID == "" || expectedAccount == "") {
		return "", errors.New("forced remote login refresh requires token ID and expected account")
	}
	if !validCallbackValue(randomSecret, 512) {
		return "", errors.New("remote login binding secret is invalid")
	}

	base, err := url.Parse(config.DefaultBaseURL)
	if err != nil {
		return "", fmt.Errorf("invalid Xiaoyunque base URL: %w", err)
	}
	base.Path = loginPagePath
	base.RawPath = ""
	base.RawQuery = ""
	base.Fragment = ""
	query := base.Query()
	query.Set("callback", callback.String())
	query.Set("random_secret_key", randomSecret)
	query.Set("source", RemoteLoginSource)
	query.Set("device_id", deviceID)
	if tokenID != "" {
		query.Set("token_id", tokenID)
	}
	if expectedAccount != "" {
		query.Set("expected_account", expectedAccount)
	}
	if forceRefresh {
		query.Set("force", "1")
	}
	base.RawQuery = query.Encode()
	return base.String(), nil
}

// ValidateRemoteAccessKeyPayload validates the browser callback binding before
// a remote MCP/OAuth service stores the returned Xiaoyunque credential.
func ValidateRemoteAccessKeyPayload(payload RemoteAccessKeyPayload, callbackURL, randomSecret string) error {
	if payload.Type != "access_key" || !validCallbackValue(payload.AccessKey, 4096) ||
		!validCallbackValue(payload.UID, 256) || !validTokenID(payload.TokenID) || payload.ExpiredAt <= 0 {
		return errors.New("invalid Xiaoyunque remote login payload")
	}
	if !constantTimeEqual(payload.RandomSecretKey, randomSecret) ||
		!constantTimeEqual(payload.Source, RemoteLoginSource) ||
		!constantTimeEqual(payload.CallbackURL, callbackURL) {
		return errors.New("Xiaoyunque remote login callback binding mismatch")
	}
	return nil
}

// CredentialFromRemotePayload converts a validated browser callback into the
// same credential shape used by the native CLI. The caller owns persistence.
func CredentialFromRemotePayload(deviceID string, payload RemoteAccessKeyPayload) *Credential {
	return &Credential{
		Version:         credentialVersion,
		DeviceID:        deviceID,
		CredentialScope: credentialScope(payload.UID, deviceID),
		AccessKey:       payload.AccessKey,
		TokenID:         payload.TokenID,
		UID:             payload.UID,
		ExpiredAt:       payload.ExpiredAt,
	}
}
