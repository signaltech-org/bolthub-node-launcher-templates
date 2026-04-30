package handler

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

type stubVerifier struct {
	allowOnce bool
	used      bool
}

func (s *stubVerifier) Verify(token, action string) error {
	if action != "finalize" {
		return errors.New("unexpected action: " + action)
	}
	if s.used {
		return errors.New("replay")
	}
	if !s.allowOnce {
		return errors.New("denied")
	}
	s.used = true
	return nil
}

func (s *stubVerifier) SignResult(b []byte) string { return "stub-mac:" + string(b[:1]) }

type stubLND struct {
	monMac      string
	invMac      string
	pairing     string
	bakeCalls   [][]MacaroonPermission
	bakeErr     error
	createErr   error
	pairingTime time.Time
}

func (s *stubLND) BakeMacaroon(_ context.Context, perms []MacaroonPermission) (string, error) {
	s.bakeCalls = append(s.bakeCalls, perms)
	if s.bakeErr != nil {
		return "", s.bakeErr
	}
	if equalPerms(perms, MonitoringPermissions) {
		return s.monMac, nil
	}
	if equalPerms(perms, InvoicesPermissions) {
		return s.invMac, nil
	}
	return "", errors.New("unexpected permissions")
}

func (s *stubLND) CreateLNCSession(_ context.Context, expiry time.Time) (string, error) {
	if s.createErr != nil {
		return "", s.createErr
	}
	s.pairingTime = expiry
	return s.pairing, nil
}

func (s *stubLND) RestoreChannelBackups(_ context.Context, _ string) error { return nil }

func equalPerms(a, b []MacaroonPermission) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func TestFinalize_HappyPath(t *testing.T) {
	v := &stubVerifier{allowOnce: true}
	l := &stubLND{monMac: "mon-hex", invMac: "inv-hex", pairing: "abandon ability ..."}
	var posted struct {
		mu  sync.Mutex
		got map[string]string
		mac string
	}
	cb := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		posted.mu.Lock()
		defer posted.mu.Unlock()
		posted.mac = r.Header.Get("X-Bolthub-Daemon-Mac")
		var p map[string]string
		_ = json.NewDecoder(r.Body).Decode(&p)
		posted.got = p
		w.WriteHeader(204)
	}))
	defer cb.Close()

	s := New("node-uuid", cb.URL, v, l)
	srv := httptest.NewServer(s.Routes())
	defer srv.Close()

	resp, err := http.Post(srv.URL+"/v1/finalize", "application/json", strings.NewReader(`{"token":"t"}`))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("status %d", resp.StatusCode)
	}
	var out FinalizeResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatal(err)
	}
	if out.PairingPhrase != "abandon ability ..." {
		t.Fatalf("pairing not returned to client")
	}
	if !out.CallbackPosted {
		t.Fatalf("callback should have been posted")
	}
	posted.mu.Lock()
	defer posted.mu.Unlock()
	if posted.got["nodeId"] != "node-uuid" || posted.got["monitoringMacaroonHex"] != "mon-hex" || posted.got["invoicesMacaroonHex"] != "inv-hex" {
		t.Fatalf("unexpected callback payload: %+v", posted.got)
	}
	if posted.mac == "" {
		t.Fatalf("callback MAC missing")
	}
}

func TestFinalize_PairingPhraseNeverInCallback(t *testing.T) {
	v := &stubVerifier{allowOnce: true}
	l := &stubLND{monMac: "mon", invMac: "inv", pairing: "secret-phrase-do-not-leak"}
	var seen string
	cb := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := readAll(r.Body)
		seen = string(body)
		w.WriteHeader(204)
	}))
	defer cb.Close()

	s := New("n", cb.URL, v, l)
	srv := httptest.NewServer(s.Routes())
	defer srv.Close()

	resp, _ := http.Post(srv.URL+"/v1/finalize", "application/json", strings.NewReader(`{"token":"t"}`))
	resp.Body.Close()

	if strings.Contains(seen, "secret-phrase-do-not-leak") {
		t.Fatalf("LNC pairing phrase leaked into bolthub callback: %s", seen)
	}
}

func TestFinalize_TokenRejected(t *testing.T) {
	v := &stubVerifier{}
	l := &stubLND{}
	s := New("n", "", v, l)
	srv := httptest.NewServer(s.Routes())
	defer srv.Close()

	resp, _ := http.Post(srv.URL+"/v1/finalize", "application/json", strings.NewReader(`{"token":"t"}`))
	resp.Body.Close()
	if resp.StatusCode != 401 {
		t.Fatalf("expected 401, got %d", resp.StatusCode)
	}
	if len(l.bakeCalls) != 0 {
		t.Fatalf("LND must not be touched when token is rejected")
	}
}

func TestFinalize_LNDFailureBubblesUp(t *testing.T) {
	v := &stubVerifier{allowOnce: true}
	l := &stubLND{bakeErr: errors.New("litd down")}
	s := New("n", "", v, l)
	srv := httptest.NewServer(s.Routes())
	defer srv.Close()
	resp, _ := http.Post(srv.URL+"/v1/finalize", "application/json", strings.NewReader(`{"token":"t"}`))
	resp.Body.Close()
	if resp.StatusCode != 502 {
		t.Fatalf("expected 502, got %d", resp.StatusCode)
	}
}

func TestFinalize_OnlyAccepts_POST(t *testing.T) {
	v := &stubVerifier{allowOnce: true}
	l := &stubLND{}
	s := New("n", "", v, l)
	srv := httptest.NewServer(s.Routes())
	defer srv.Close()

	resp, _ := http.Get(srv.URL + "/v1/finalize")
	resp.Body.Close()
	if resp.StatusCode != 405 {
		t.Fatalf("expected 405, got %d", resp.StatusCode)
	}
}

// readAll without pulling io into test surface
func readAll(r interface{ Read(p []byte) (int, error) }) ([]byte, error) {
	buf := make([]byte, 0, 256)
	tmp := make([]byte, 256)
	for {
		n, err := r.Read(tmp)
		if n > 0 {
			buf = append(buf, tmp[:n]...)
		}
		if err != nil {
			if err.Error() == "EOF" {
				return buf, nil
			}
			return buf, err
		}
	}
}
