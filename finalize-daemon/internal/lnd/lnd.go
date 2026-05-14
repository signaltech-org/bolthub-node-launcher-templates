// Package lnd is a tiny REST client to the local litd container that runs
// alongside the finalize daemon. The daemon is the only process inside the
// VM that holds the wallet macaroons, so it is the single chokepoint for
// "bake another scoped macaroon" and "create a new LNC pairing" RPCs.
//
// Everything in this package talks plain HTTPS to the LND REST gateway
// (--lnd.restlisten, default https://127.0.0.1:8080) and tolerates litd's
// self-signed cert. The surface area is intentionally minimal so it can be
// audited end-to-end on the VM.
//
// Two macaroons are required because litd's REST gateway delegates
// authentication per subserver: LND endpoints (e.g. /v1/macaroon for
// BakeMacaroon) are validated by LND's macaroon root key, but
// litd-specific endpoints (e.g. /v1/sessions for CreateLNCSession) are
// validated by litd's separate root key. A single macaroon cannot pass
// both validators, so the Client carries both and sends the appropriate
// one per call.
package lnd

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"
)

// Client is a stateless wrapper around the litd REST endpoints we need.
type Client struct {
	BaseURL    string
	HTTPClient *http.Client
	// lndMacaroon is sent with calls to LND-native REST endpoints (e.g.
	// /v1/macaroon for BakeMacaroon, /v1/channels/backup/restore for
	// RestoreChannelBackups). Validated against LND's root key.
	lndMacaroon string
	// litdMacaroon is sent with calls to litd-specific REST endpoints
	// (e.g. /v1/sessions for CreateLNCSession). Validated against litd's
	// separate root key.
	litdMacaroon string
}

// New constructs a Client for the local litd. Loads two macaroons from
// disk: an LND admin macaroon (used for LND-native REST calls) and a
// litd super-macaroon (used for litd-specific REST calls). See the
// package comment for why both are required.
//
// In integrated mode the standard locations on the host (with the litd
// docker compose volume names this template uses) are:
//
//	/var/lib/docker/volumes/litd_lnd-data/_data/data/chain/bitcoin/<network>/admin.macaroon
//	/var/lib/docker/volumes/litd_lit-data/_data/<network>/lit.macaroon
func New(baseURL, lndMacaroonPath, litdMacaroonPath string) (*Client, error) {
	lndMac, err := os.ReadFile(lndMacaroonPath)
	if err != nil {
		return nil, fmt.Errorf("lnd.New: read LND macaroon at %s: %w", lndMacaroonPath, err)
	}
	litdMac, err := os.ReadFile(litdMacaroonPath)
	if err != nil {
		return nil, fmt.Errorf("lnd.New: read litd macaroon at %s: %w", litdMacaroonPath, err)
	}
	tr := &http.Transport{
		TLSClientConfig: &tls.Config{InsecureSkipVerify: true},
	}
	return &Client{
		BaseURL:      strings.TrimRight(baseURL, "/"),
		HTTPClient:   &http.Client{Timeout: 30 * time.Second, Transport: tr},
		lndMacaroon:  hex.EncodeToString(lndMac),
		litdMacaroon: hex.EncodeToString(litdMac),
	}, nil
}

// MacaroonPermission mirrors lnrpc.MacaroonPermission.
type MacaroonPermission struct {
	Entity string `json:"entity"`
	Action string `json:"action"`
}

// MonitoringPermissions are read-only telemetry permissions handed to the
// bolthub API for dashboards / health checks. Mirrors the TS definition in
// packages/node-provisioner/src/ssh.ts MONITORING_PERMISSIONS.
var MonitoringPermissions = []MacaroonPermission{
	{Entity: "info", Action: "read"},
	{Entity: "offchain", Action: "read"},
	{Entity: "onchain", Action: "read"},
	{Entity: "peers", Action: "read"},
}

// InvoicesPermissions are mint-only L402 invoice issuance permissions.
// Mirrors INVOICES_PERMISSIONS in TS.
var InvoicesPermissions = []MacaroonPermission{
	{Entity: "info", Action: "read"},
	{Entity: "invoices", Action: "read"},
	{Entity: "invoices", Action: "write"},
}

// LncBrowserPermissions is the minimal LND permission set the bolthub
// dashboard needs over the user's browser-held LNC session. Used as
// `macaroon_custom_permissions` when minting the bolthub-finalize
// session via litd's /v1/sessions.
//
// Included (what the dashboard actually does):
//   - info:read     — getInfo (verify pairing, alias / pubkey display)
//   - address:write — newAddress for on-chain deposit
//   - peers:read    — listPeers confirms LSP peer connect succeeded
//   - peers:write   — connectPeer to the LSP before channel open
//   - offchain:read — exportAllChannelBackups for SCB autobackup
//   - onchain:read  — view-only on-chain state (used by future
//                     surfaces; harmless to grant)
//
// Deliberately NOT included:
//   - onchain:write  — would let the browser send the user's on-chain
//                      balance to an arbitrary address. The dashboard
//                      never needs this.
//   - offchain:write — pay invoices / force-close channels. Same
//                      reasoning.
//   - macaroon:*     — re-baking new macaroons could escalate scope.
//   - signer:*       — signing arbitrary messages is a step toward
//                      spending in several LND code paths.
//
// Users who want full-admin access (e.g. import the pairing into Zeus
// mobile as their primary wallet UI) should mint a separate
// TYPE_MACAROON_ADMIN session from the LIT Web UI themselves. The
// bolthub-finalize session is for the dashboard only.
var LncBrowserPermissions = []MacaroonPermission{
	{Entity: "info", Action: "read"},
	{Entity: "address", Action: "write"},
	{Entity: "peers", Action: "read"},
	{Entity: "peers", Action: "write"},
	{Entity: "offchain", Action: "read"},
	{Entity: "onchain", Action: "read"},
}

type bakeReq struct {
	Permissions []MacaroonPermission `json:"permissions"`
}

type bakeResp struct {
	Macaroon string `json:"macaroon"`
}

// BakeMacaroon calls /v1/macaroon and returns the resulting macaroon as a
// hex string (matching what BoltHub already expects on the wire).
func (c *Client) BakeMacaroon(ctx context.Context, perms []MacaroonPermission) (string, error) {
	body, _ := json.Marshal(bakeReq{Permissions: perms})
	req, _ := http.NewRequestWithContext(ctx, "POST", c.BaseURL+"/v1/macaroon", bytes.NewReader(body))
	req.Header.Set("Grpc-Metadata-macaroon", c.lndMacaroon)
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.HTTPClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("BakeMacaroon: %w", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 {
		return "", fmt.Errorf("BakeMacaroon: status %d: %s", resp.StatusCode, string(raw))
	}
	var out bakeResp
	if err := json.Unmarshal(raw, &out); err != nil {
		return "", fmt.Errorf("BakeMacaroon: bad response: %w", err)
	}
	if out.Macaroon == "" {
		return "", errors.New("BakeMacaroon: empty macaroon in response")
	}
	// LND's BakeMacaroonResponse proto declares the field as
	// "hex encoded macaroon, serialized in binary format" — i.e. the REST
	// gateway returns hex, not base64. The previous implementation
	// attempted base64 decode first and only fell back on parse failure,
	// but hex strings are a subset of the base64 alphabet (`[a-fA-F0-9]`),
	// so base64 decode would silently "succeed" on a hex input and produce
	// unrelated bytes. The downstream `hex.EncodeToString` then re-encoded
	// those garbage bytes, and the macaroon BoltHub stored could never
	// authenticate against LND ("cannot determine data format of
	// binary-encoded macaroon" on every subsequent /v1/getinfo call).
	//
	// Detect hex first by length parity + alphabet, fall back to base64
	// for older / non-LND gateways that genuinely return base64, and only
	// pass through unchanged if both fail.
	if isHexString(out.Macaroon) {
		return out.Macaroon, nil
	}
	if macBytes, err := base64.StdEncoding.DecodeString(out.Macaroon); err == nil {
		return hex.EncodeToString(macBytes), nil
	}
	return out.Macaroon, nil
}

// isHexString reports whether s is a non-empty even-length string of hex
// digits. A stricter test than `hex.DecodeString` because it rejects empty
// strings and is cheap to compose with the base64-fallback path.
func isHexString(s string) bool {
	if len(s) == 0 || len(s)%2 != 0 {
		return false
	}
	_, err := hex.DecodeString(s)
	return err == nil
}

type addSessionReq struct {
	Label                     string                 `json:"label"`
	SessionType               string                 `json:"session_type"`
	ExpiryTimestampSeconds    string                 `json:"expiry_timestamp_seconds"`
	MailboxServerAddr         string                 `json:"mailbox_server_addr"`
	DevServer                 bool                   `json:"dev_server"`
	MacaroonCustomPermissions []MacaroonPermission   `json:"macaroon_custom_permissions"`
	AccountID                 string                 `json:"account_id,omitempty"`
	Extra                     map[string]interface{} `json:"-"`
}

type addSessionResp struct {
	Session struct {
		PairingSecretMnemonic string `json:"pairing_secret_mnemonic"`
		LocalPublicKey        string `json:"local_public_key"`
	} `json:"session"`
}

// restoreReq mirrors LND's RestoreChanBackupRequest. The body of a
// multi-channel backup is base64-encoded bytes per LND's REST gateway.
type restoreReq struct {
	MultiChanBackup string `json:"multi_chan_backup"`
}

// RestoreChannelBackups pushes a Static Channel Backup blob into the
// local litd. LND will fan out per-channel close requests to each peer
// listed in the backup; funds settle on-chain once peers cooperate (or
// after the cltv timeout). The blob is opaque to the daemon — it is
// encrypted by a key derived from the wallet seed and only decryptable
// by the live LND wallet that holds it.
//
// Phase 5.6: this is the chokepoint for the one-click recovery flow. The
// browser supplies the SCB blob; BoltHub never holds it (only an
// encrypted opt-in copy via cloud sync, which the user can decline).
func (c *Client) RestoreChannelBackups(ctx context.Context, multiBlobBase64 string) error {
	body, _ := json.Marshal(restoreReq{MultiChanBackup: multiBlobBase64})
	req, _ := http.NewRequestWithContext(ctx, "POST", c.BaseURL+"/v1/channels/backup/restore", bytes.NewReader(body))
	req.Header.Set("Grpc-Metadata-macaroon", c.lndMacaroon)
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.HTTPClient.Do(req)
	if err != nil {
		return fmt.Errorf("RestoreChannelBackups: %w", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 {
		return fmt.Errorf("RestoreChannelBackups: status %d: %s", resp.StatusCode, string(raw))
	}
	return nil
}

// CreateLNCSession asks litd to mint a custom-scoped LNC pairing for
// the bolthub dashboard's exclusive use. Scope is constrained via
// `LncBrowserPermissions` — see its doc comment for the rationale.
// The pairing phrase is returned to the daemon's caller (the browser)
// and is never echoed home to bolthub.
//
// Users who need full admin access (Zeus as primary wallet UI etc)
// should mint their own TYPE_MACAROON_ADMIN session from the LIT Web
// UI on port 8443. The pairing handed back here is intentionally not
// admin-scoped so that a compromised browser localStorage cannot
// drain on-chain or force-close channels.
func (c *Client) CreateLNCSession(ctx context.Context, expiry time.Time) (string, error) {
	req := addSessionReq{
		Label:                     "bolthub-finalize",
		SessionType:               "TYPE_MACAROON_CUSTOM",
		ExpiryTimestampSeconds:    fmt.Sprintf("%d", expiry.Unix()),
		MailboxServerAddr:         "mailbox.terminal.lightning.today:443",
		DevServer:                 false,
		MacaroonCustomPermissions: LncBrowserPermissions,
	}
	body, _ := json.Marshal(req)
	httpReq, _ := http.NewRequestWithContext(ctx, "POST", c.BaseURL+"/v1/sessions", bytes.NewReader(body))
	// /v1/sessions is a litd subserver endpoint; LND's root key won't
	// validate against it. Use the litd super-macaroon here.
	httpReq.Header.Set("Grpc-Metadata-macaroon", c.litdMacaroon)
	httpReq.Header.Set("Content-Type", "application/json")

	resp, err := c.HTTPClient.Do(httpReq)
	if err != nil {
		return "", fmt.Errorf("CreateLNCSession: %w", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 {
		return "", fmt.Errorf("CreateLNCSession: status %d: %s", resp.StatusCode, string(raw))
	}
	var out addSessionResp
	if err := json.Unmarshal(raw, &out); err != nil {
		return "", fmt.Errorf("CreateLNCSession: bad response: %w", err)
	}
	if out.Session.PairingSecretMnemonic == "" {
		return "", errors.New("CreateLNCSession: empty pairing mnemonic")
	}
	return out.Session.PairingSecretMnemonic, nil
}
