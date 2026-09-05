package publicapp

import (
	"bytes"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestEnvelopeCryptoRejectsTamperAndWrongAccount(t *testing.T) {
	v, e := newVault(bytes.Repeat([]byte{1}, 32))
	if e != nil {
		t.Fatal(e)
	}
	plain := []byte("secret-not-for-model")
	sealed := v.seal("account-a", plain)
	if bytes.Contains(sealed, plain) {
		t.Fatal("plaintext exposed")
	}
	opened, e := v.open("account-a", sealed)
	if e != nil || !bytes.Equal(opened, plain) {
		t.Fatal("roundtrip failed")
	}
	if _, e = v.open("account-b", sealed); e == nil {
		t.Fatal("ciphertext transplant accepted")
	}
	sealed[len(sealed)-1] ^= 1
	if _, e = v.open("account-a", sealed); e == nil {
		t.Fatal("tampering accepted")
	}
	other, _ := newVault(bytes.Repeat([]byte{2}, 32))
	if _, e = other.open("account-a", sealed); e == nil {
		t.Fatal("wrong key accepted")
	}
}
func TestPKCERFC7636(t *testing.T) {
	v := "dBjftJeZ4CVP-mB92K27uhbUJU1p1r_wW1gFWFOEjXk"
	if !pkceValid(v) || challenge(v) != "E9Melhoa2OwvFrEMTJguCHaoeK1t8URWbuGJSstw-cM" {
		t.Fatal("RFC vector failed")
	}
	for _, bad := range []string{"", strings.Repeat("x", 42), strings.Repeat("x", 129), strings.Repeat("x", 42) + " ", strings.Repeat("x", 42) + "é"} {
		if pkceValid(bad) {
			t.Errorf("accepted invalid verifier %q", bad)
		}
	}
}
func TestFormsRejectDuplicateTokens(t *testing.T) {
	r := httptest.NewRequest("POST", "https://app.test/oauth/token", strings.NewReader("code=one&code=two"))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	w := httptest.NewRecorder()
	if parseForm(w, r) || w.Code != 400 {
		t.Fatal("duplicate accepted")
	}
}
func TestNestedOwnershipReferences(t *testing.T) {
	refs := resourceRefs(map[string]any{"thread_id": "ta", "run_id": "rb", "request": map[string]any{"root_pippit_asset_id": "root", "transactions": []any{map[string]any{"patches": []any{map[string]any{"asset_id": "foreign"}}}}}})
	found := map[string]resourceRef{}
	for _, r := range refs {
		found[r.id] = r
	}
	if found["rb"].parent != "ta" || found["foreign"].kind != "asset" || found["root"].kind != "asset" {
		t.Fatal(refs)
	}
}
