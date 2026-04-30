package tokens

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"path/filepath"
	"testing"
	"time"
)

func newVerifier(t *testing.T, secret []byte) *Verifier {
	t.Helper()
	dir := t.TempDir()
	v, err := New(secret, filepath.Join(dir, "consumed.log"))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return v
}

func TestVerify_Happy(t *testing.T) {
	secret := []byte("super-secret")
	v := newVerifier(t, secret)
	now := time.Date(2026, 4, 30, 0, 0, 0, 0, time.UTC)
	v.SetClock(func() time.Time { return now })

	tok := mintTokenInline(secret, "finalize", now.Add(10*time.Minute).Unix(), "abc123")
	if err := v.Verify(tok, "finalize"); err != nil {
		t.Fatalf("Verify: %v", err)
	}
}

func TestVerify_Replay(t *testing.T) {
	secret := []byte("super-secret")
	v := newVerifier(t, secret)
	now := time.Date(2026, 4, 30, 0, 0, 0, 0, time.UTC)
	v.SetClock(func() time.Time { return now })

	tok := mintTokenInline(secret, "finalize", now.Add(10*time.Minute).Unix(), "abc123")
	if err := v.Verify(tok, "finalize"); err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if err := v.Verify(tok, "finalize"); err == nil {
		t.Fatal("expected replay to be rejected")
	}
}

func TestVerify_BadSignature(t *testing.T) {
	v := newVerifier(t, []byte("real"))
	tok := mintTokenInline([]byte("attacker"), "finalize", time.Now().Add(10*time.Minute).Unix(), "abc123")
	if err := v.Verify(tok, "finalize"); err == nil {
		t.Fatal("expected signature mismatch")
	}
}

func TestVerify_Expired(t *testing.T) {
	secret := []byte("k")
	v := newVerifier(t, secret)
	now := time.Date(2026, 4, 30, 0, 0, 0, 0, time.UTC)
	v.SetClock(func() time.Time { return now })
	tok := mintTokenInline(secret, "finalize", now.Add(-1*time.Minute).Unix(), "n")
	if err := v.Verify(tok, "finalize"); err == nil {
		t.Fatal("expected expiry rejection")
	}
}

func TestVerify_ActionMismatch(t *testing.T) {
	secret := []byte("k")
	v := newVerifier(t, secret)
	tok := mintTokenInline(secret, "finalize", time.Now().Add(time.Minute).Unix(), "n")
	if err := v.Verify(tok, "purge"); err == nil {
		t.Fatal("expected action mismatch")
	}
}

func TestPersistenceAcrossRestart(t *testing.T) {
	secret := []byte("k")
	dir := t.TempDir()
	store := filepath.Join(dir, "consumed.log")

	v1, err := New(secret, store)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	tok := mintTokenInline(secret, "finalize", time.Now().Add(time.Minute).Unix(), "n")
	if err := v1.Verify(tok, "finalize"); err != nil {
		t.Fatal(err)
	}

	// Simulate restart by reloading from the same store file
	v2, err := New(secret, store)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if err := v2.Verify(tok, "finalize"); err == nil {
		t.Fatal("expected replay rejection across restart")
	}
}

func mintTokenInline(secret []byte, action string, expiresUnix int64, nonce string) string {
	body := []byte(action + "." + itoa(expiresUnix) + "." + nonce)
	mac := hmac.New(sha256.New, secret)
	mac.Write(body)
	return string(body) + "." + hex.EncodeToString(mac.Sum(nil))
}

func itoa(i int64) string {
	// avoid strconv import in test helpers
	if i == 0 {
		return "0"
	}
	neg := false
	if i < 0 {
		neg = true
		i = -i
	}
	digits := []byte{}
	for i > 0 {
		digits = append([]byte{byte('0' + i%10)}, digits...)
		i /= 10
	}
	if neg {
		return "-" + string(digits)
	}
	return string(digits)
}
