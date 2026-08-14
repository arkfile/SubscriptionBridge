package testdata

import (
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

type Fixture struct {
	Fixture        string `json:"fixture"`
	FixtureVersion int    `json:"fixture_version"`
	PairingRoot    struct {
		ConfiguredHex    string `json:"configured_hex"`
		DecodedBytesHex  string `json:"decoded_bytes_hex"`
		SaltASCII        string `json:"salt_ascii"`
		Keys             struct {
			Token struct {
				InfoASCII     string `json:"info_ascii"`
				DerivedKeyHex string `json:"derived_key_hex"`
			} `json:"token"`
			Callback struct {
				InfoASCII     string `json:"info_ascii"`
				DerivedKeyHex string `json:"derived_key_hex"`
			} `json:"callback"`
			Reconcile struct {
				InfoASCII     string `json:"info_ascii"`
				DerivedKeyHex string `json:"derived_key_hex"`
			} `json:"reconcile"`
		} `json:"keys"`
	} `json:"pairing_root"`
	StartToken struct {
		PayloadJSONUTF8  string `json:"payload_json_utf8"`
		PayloadBase64URL string `json:"payload_base64url"`
		SignatureHex     string `json:"signature_hex"`
		Token            string `json:"token"`
	} `json:"start_token"`
	PortalToken struct {
		PayloadJSONUTF8  string `json:"payload_json_utf8"`
		PayloadBase64URL string `json:"payload_base64url"`
		SignatureHex     string `json:"signature_hex"`
		Token            string `json:"token"`
	} `json:"portal_token"`
	Callback struct {
		BodyJSONUTF8      string `json:"body_json_utf8"`
		SignatureTimestamp int64  `json:"signature_timestamp"`
		SignatureBaseUTF8 string `json:"signature_base_utf8"`
		SignatureHex      string `json:"signature_hex"`
		SignatureHeader   string `json:"signature_header"`
	} `json:"callback"`
	ReconciliationRequest struct {
		Method             string `json:"method"`
		Path               string `json:"path"`
		SignatureTimestamp int64  `json:"signature_timestamp"`
		SignatureBaseUTF8  string `json:"signature_base_utf8"`
		SignatureHex       string `json:"signature_hex"`
		AuthorizationHeader string `json:"authorization_header"`
	} `json:"reconciliation_request"`
	Snapshot struct {
		ResponseStatus                int    `json:"response_status"`
		ContentType                   string `json:"content_type"`
		ResponseIsIndependentlySigned bool   `json:"response_is_independently_signed"`
		BodyJSONUTF8                  string `json:"body_json_utf8"`
	} `json:"snapshot"`
}

// Load reads fixtures/protocol-v1.json from the module root.
func Load(t *testing.T) Fixture {
	t.Helper()
	path := filepath.Join(ModuleRoot(t), "fixtures", "protocol-v1.json")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	var fx Fixture
	if err := json.Unmarshal(raw, &fx); err != nil {
		t.Fatalf("parse fixture: %v", err)
	}
	return fx
}

// ModuleRoot walks up from this file to the directory containing go.mod.
func ModuleRoot(t *testing.T) string {
	t.Helper()
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	dir := filepath.Dir(file)
	for i := 0; i < 8; i++ {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			break
		}
		dir = parent
	}
	t.Fatal("module root not found")
	return ""
}
