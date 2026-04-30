// Package handler wires the finalize daemon HTTP routes to its dependencies
// (token verifier, lnd client, callback poster). Endpoints are intentionally
// minimal — the daemon only exists so the bolthub server never has to SSH
// into the node — and every endpoint is gated by a single-use, HMAC-signed
// token issued by the bolthub API.
package handler

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"time"
)

// LND is the subset of lnd.Client the handler needs; an interface so we can
// test handlers without standing up a live litd container.
type LND interface {
	BakeMacaroon(ctx context.Context, perms []MacaroonPermission) (string, error)
	CreateLNCSession(ctx context.Context, expiry time.Time) (string, error)
	// Phase 5.6: push a Static Channel Backup blob to LND. Used by the
	// one-click recovery flow after the user has restored their wallet
	// from seed. The daemon never holds the blob — it is supplied by
	// the browser at recovery time.
	RestoreChannelBackups(ctx context.Context, multiBlobBase64 string) error
}

// MacaroonPermission mirrors lnrpc.MacaroonPermission.
type MacaroonPermission struct {
	Entity string `json:"entity"`
	Action string `json:"action"`
}

// Re-exported so handler tests can reference them by value.
var (
	MonitoringPermissions = []MacaroonPermission{
		{Entity: "info", Action: "read"},
		{Entity: "offchain", Action: "read"},
		{Entity: "onchain", Action: "read"},
		{Entity: "peers", Action: "read"},
	}
	InvoicesPermissions = []MacaroonPermission{
		{Entity: "info", Action: "read"},
		{Entity: "invoices", Action: "read"},
		{Entity: "invoices", Action: "write"},
	}
)

// TokenVerifier is the subset of tokens.Verifier the handler uses.
type TokenVerifier interface {
	Verify(token string, expectedAction string) error
	SignResult(payload []byte) string
}

// PasswordVerifier is the subset of argon2pw.Hash the handler uses, so we
// can swap in a fake in tests.
type PasswordVerifier interface {
	Verify(plaintext string) error
}

// Server bundles the daemon's runtime dependencies.
type Server struct {
	NodeID             string
	BoltHubCallbackURL string
	HTTPClient         *http.Client
	Tokens             TokenVerifier
	Lnd                LND
	// PasswordHash is set when bolthub embedded an Argon2id hash of the
	// user's litd UI password. nil means the legacy server-generated
	// bootstrap password is in use and the daemon does not gate finalize
	// on a plaintext check.
	PasswordHash PasswordVerifier
}

// New returns a Server with sane defaults.
func New(nodeID, callbackURL string, tokens TokenVerifier, lnd LND) *Server {
	return &Server{
		NodeID:             nodeID,
		BoltHubCallbackURL: callbackURL,
		HTTPClient: &http.Client{
			Timeout: 30 * time.Second,
			Transport: &http.Transport{
				TLSClientConfig: &tls.Config{MinVersion: tls.VersionTLS12},
			},
		},
		Tokens: tokens,
		Lnd:    lnd,
	}
}

// Routes returns an http.Handler with the daemon's endpoints mounted.
func (s *Server) Routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", s.handleHealth)
	mux.HandleFunc("/v1/finalize", s.handleFinalize)
	mux.HandleFunc("/v1/verify-password", s.handleVerifyPassword)
	mux.HandleFunc("/v1/recover-scb", s.handleRecoverScb)
	return mux
}

func (s *Server) handleHealth(w http.ResponseWriter, _ *http.Request) {
	w.WriteHeader(200)
	_, _ = w.Write([]byte(`{"status":"ok"}`))
}

type finalizeReq struct {
	Token string `json:"token"`
	// Optional plaintext for the user-set litd UI password. When the
	// daemon was started with an Argon2id hash (Phase 3.2) it requires
	// this field and rejects finalize if the hash does not match. When no
	// hash is present, the legacy bootstrap-password flow is in effect
	// and this field is ignored.
	LitdPassword string `json:"litdPassword,omitempty"`
}

type verifyPasswordReq struct {
	Token        string `json:"token"`
	LitdPassword string `json:"litdPassword"`
}

// recoverScbReq carries the Static Channel Backup blob the browser
// uploads during the recovery flow. The blob is encrypted by the
// wallet seed so the daemon cannot read it; it is forwarded as-is to
// LND's `RestoreChannelBackups` RPC, which asks each peer to
// cooperatively close so funds settle on-chain.
type recoverScbReq struct {
	Token       string `json:"token"`
	BlobBase64  string `json:"blobBase64"`
}

// FinalizeResponse is what the daemon returns to the browser. The
// pairingPhrase is only delivered here; bolthub never sees it.
type FinalizeResponse struct {
	NodeID         string `json:"nodeId"`
	PairingPhrase  string `json:"lncPairingPhrase"`
	CallbackPosted bool   `json:"callbackPosted"`
}

// callbackPayload is what the daemon POSTs to bolthub once it has baked
// the scoped macaroons. The MAC is computed over the JSON bytes using the
// shared per-node webhook secret so a third party with the daemon URL
// cannot poison bolthub's macaroon storage.
type callbackPayload struct {
	NodeID                string `json:"nodeId"`
	MonitoringMacaroonHex string `json:"monitoringMacaroonHex"`
	InvoicesMacaroonHex   string `json:"invoicesMacaroonHex"`
}

func (s *Server) handleFinalize(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var req finalizeReq
	if err := json.NewDecoder(io.LimitReader(r.Body, 16*1024)).Decode(&req); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	if err := s.Tokens.Verify(req.Token, "finalize"); err != nil {
		log.Printf("[finalize-daemon] token rejected: %v", err)
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}

	if s.PasswordHash != nil {
		if req.LitdPassword == "" {
			http.Error(w, "missing litd password", http.StatusBadRequest)
			return
		}
		if err := s.PasswordHash.Verify(req.LitdPassword); err != nil {
			log.Printf("[finalize-daemon] password mismatch: %v", err)
			http.Error(w, "wrong litd password", http.StatusUnauthorized)
			return
		}
	}

	ctx, cancel := context.WithTimeout(r.Context(), 60*time.Second)
	defer cancel()

	monMac, err := s.Lnd.BakeMacaroon(ctx, MonitoringPermissions)
	if err != nil {
		log.Printf("[finalize-daemon] bake monitoring: %v", err)
		http.Error(w, "lnd: bake monitoring failed", http.StatusBadGateway)
		return
	}
	invMac, err := s.Lnd.BakeMacaroon(ctx, InvoicesPermissions)
	if err != nil {
		log.Printf("[finalize-daemon] bake invoices: %v", err)
		http.Error(w, "lnd: bake invoices failed", http.StatusBadGateway)
		return
	}
	pairing, err := s.Lnd.CreateLNCSession(ctx, time.Now().AddDate(10, 0, 0))
	if err != nil {
		log.Printf("[finalize-daemon] create lnc: %v", err)
		http.Error(w, "lnd: create lnc failed", http.StatusBadGateway)
		return
	}

	posted := true
	if err := s.postCallback(ctx, callbackPayload{
		NodeID:                s.NodeID,
		MonitoringMacaroonHex: monMac,
		InvoicesMacaroonHex:   invMac,
	}); err != nil {
		log.Printf("[finalize-daemon] callback: %v", err)
		posted = false
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(FinalizeResponse{
		NodeID:         s.NodeID,
		PairingPhrase:  pairing,
		CallbackPosted: posted,
	})
}

// handleVerifyPassword lets the browser confirm the user's plaintext
// against the embedded Argon2id hash without committing to a finalize
// (useful for Phase 5 onboarding wizards that want to validate the user
// typed it correctly before triggering the destructive part). Token must
// still be a valid one-time finalize token; reusing it does not bypass
// the replay store.
func (s *Server) handleVerifyPassword(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var req verifyPasswordReq
	if err := json.NewDecoder(io.LimitReader(r.Body, 16*1024)).Decode(&req); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	if err := s.Tokens.Verify(req.Token, "verify-password"); err != nil {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	if s.PasswordHash == nil {
		// Nothing to verify against; surface a 404-like signal so the
		// browser can hide the password-confirm UI on legacy nodes.
		http.Error(w, "no password hash configured", http.StatusNotFound)
		return
	}
	if err := s.PasswordHash.Verify(req.LitdPassword); err != nil {
		http.Error(w, "wrong litd password", http.StatusUnauthorized)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write([]byte(`{"ok":true}`))
}

// handleRecoverScb takes a base64 SCB blob from the browser and pushes
// it into the local LND. The browser already has the SCB (either kept
// in localStorage when channels were live, or downloaded from
// BoltHub's optional cloud sync). The seed is never sent to BoltHub
// during recovery — the user restores their wallet from seed via litd's
// own UI before invoking this endpoint.
func (s *Server) handleRecoverScb(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	// 256 KB cap matches the BoltHub cloud-sync upload limit; the SCB
	// for a sane node is a few KB.
	var req recoverScbReq
	if err := json.NewDecoder(io.LimitReader(r.Body, 384*1024)).Decode(&req); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	if err := s.Tokens.Verify(req.Token, "recover-scb"); err != nil {
		log.Printf("[finalize-daemon] recover token rejected: %v", err)
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	if req.BlobBase64 == "" {
		http.Error(w, "missing blob", http.StatusBadRequest)
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 60*time.Second)
	defer cancel()
	if err := s.Lnd.RestoreChannelBackups(ctx, req.BlobBase64); err != nil {
		log.Printf("[finalize-daemon] restore scb: %v", err)
		http.Error(w, "lnd: restore failed", http.StatusBadGateway)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write([]byte(`{"ok":true}`))
}

func (s *Server) postCallback(ctx context.Context, p callbackPayload) error {
	if s.BoltHubCallbackURL == "" {
		return errors.New("no callback URL configured")
	}
	body, err := json.Marshal(p)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, "POST", s.BoltHubCallbackURL, newReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Bolthub-Daemon-Mac", s.Tokens.SignResult(body))
	req.Header.Set("X-Bolthub-Node-Id", s.NodeID)

	resp, err := s.HTTPClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		raw, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("status %d: %s", resp.StatusCode, string(raw))
	}
	return nil
}

// newReader avoids pulling bytes into the package surface area for callers.
func newReader(b []byte) io.Reader { return &readonly{b: b} }

type readonly struct {
	b []byte
	o int
}

func (r *readonly) Read(p []byte) (int, error) {
	if r.o >= len(r.b) {
		return 0, io.EOF
	}
	n := copy(p, r.b[r.o:])
	r.o += n
	return n, nil
}
