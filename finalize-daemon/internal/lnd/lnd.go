// Package lnd is a tiny REST client to the local litd container that runs
// alongside the finalize daemon. The daemon is the only process inside the
// VM that holds the litd admin macaroon (read out of /root/.lit at startup
// via a bind mount), so it is the single chokepoint for "bake another
// scoped macaroon" and "create a new LNC pairing" RPCs.
//
// Everything in this package talks plain HTTPS to https://127.0.0.1:8443
// and tolerates the litd self-signed cert; the surface area is intentionally
// minimal so it can be audited end-to-end on the VM.
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
	macaroon   string
}

// New constructs a Client for the local litd, loading the admin macaroon
// from path. macaroonPath is /root/.lit/admin.macaroon by default.
func New(baseURL string, macaroonPath string) (*Client, error) {
	mac, err := os.ReadFile(macaroonPath)
	if err != nil {
		return nil, fmt.Errorf("lnd.New: read macaroon: %w", err)
	}
	tr := &http.Transport{
		TLSClientConfig: &tls.Config{InsecureSkipVerify: true},
	}
	return &Client{
		BaseURL:    strings.TrimRight(baseURL, "/"),
		HTTPClient: &http.Client{Timeout: 30 * time.Second, Transport: tr},
		macaroon:   hex.EncodeToString(mac),
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
	req.Header.Set("Grpc-Metadata-macaroon", c.macaroon)
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
	// litd returns base64 here despite the field name; convert to hex like
	// the bolthub TS expects so the wire format is identical to the SSH path.
	macBytes, err := base64.StdEncoding.DecodeString(out.Macaroon)
	if err != nil {
		// Some versions return hex directly; pass through.
		return out.Macaroon, nil
	}
	return hex.EncodeToString(macBytes), nil
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
	req.Header.Set("Grpc-Metadata-macaroon", c.macaroon)
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

// CreateLNCSession asks litd to mint an admin LNC pairing the user can
// import into Zeus / Lightning Terminal directly. The pairing phrase is
// returned to the daemon's caller (the browser) and is never echoed home
// to bolthub.
func (c *Client) CreateLNCSession(ctx context.Context, expiry time.Time) (string, error) {
	req := addSessionReq{
		Label:                  "bolthub-finalize",
		SessionType:            "TYPE_MACAROON_ADMIN",
		ExpiryTimestampSeconds: fmt.Sprintf("%d", expiry.Unix()),
		MailboxServerAddr:      "mailbox.terminal.lightning.today:443",
		DevServer:              false,
	}
	body, _ := json.Marshal(req)
	httpReq, _ := http.NewRequestWithContext(ctx, "POST", c.BaseURL+"/v1/sessions", bytes.NewReader(body))
	httpReq.Header.Set("Grpc-Metadata-macaroon", c.macaroon)
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
