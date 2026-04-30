package argon2pw

import (
	"encoding/hex"
	"os"
	"path/filepath"
	"testing"

	"golang.org/x/crypto/argon2"
)

func writeHash(t *testing.T, plaintext string) string {
	t.Helper()
	salt := []byte("0123456789abcdef")
	h := argon2.IDKey([]byte(plaintext), salt, 3, 64*1024, 1, 32)
	body := []byte(`{"hashHex":"` + hex.EncodeToString(h) + `","saltHex":"` + hex.EncodeToString(salt) + `","params":{"t":3,"m":65536,"p":1}}`)

	dir := t.TempDir()
	path := filepath.Join(dir, "litd-password.hash")
	if err := os.WriteFile(path, body, 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestLoadAndVerify(t *testing.T) {
	path := writeHash(t, "correct horse battery staple")
	h, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if h == nil {
		t.Fatal("nil hash")
	}
	if err := h.Verify("correct horse battery staple"); err != nil {
		t.Fatalf("expected verify ok, got %v", err)
	}
	if err := h.Verify("nope"); err == nil {
		t.Fatal("expected mismatch error")
	}
}

func TestLoadMissingReturnsNil(t *testing.T) {
	h, err := Load("")
	if err != nil || h != nil {
		t.Fatalf("expected (nil,nil), got (%v,%v)", h, err)
	}
	h, err = Load("/no/such/file/abc")
	if err != nil || h != nil {
		t.Fatalf("expected (nil,nil), got (%v,%v)", h, err)
	}
}

func TestLoadIncomplete(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "x.json")
	_ = os.WriteFile(path, []byte(`{}`), 0o600)
	_, err := Load(path)
	if err == nil {
		t.Fatal("expected error for incomplete hash")
	}
}
