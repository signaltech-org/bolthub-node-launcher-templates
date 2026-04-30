// Package argon2pw verifies a plaintext litd UI password against the
// browser-supplied Argon2id hash that cloud-init parked at
// /opt/bolthub/litd-password.hash. Bolthub never sees the plaintext; the
// daemon is the only place on the path between the user's browser and
// litd.
package argon2pw

import (
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"

	"golang.org/x/crypto/argon2"
)

// Hash is the JSON shape written by cloud-init.
type Hash struct {
	HashHex string `json:"hashHex"`
	SaltHex string `json:"saltHex"`
	Params  struct {
		T uint32 `json:"t"`
		M uint32 `json:"m"`
		P uint8  `json:"p"`
	} `json:"params"`
}

// Load reads and JSON-decodes the hash file. Empty path is allowed and
// returns (nil, nil) so the caller can opt-out cleanly.
func Load(path string) (*Hash, error) {
	if path == "" {
		return nil, nil
	}
	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, fmt.Errorf("argon2pw.Load: %w", err)
	}
	var h Hash
	if err := json.Unmarshal(data, &h); err != nil {
		return nil, fmt.Errorf("argon2pw.Load: parse: %w", err)
	}
	if h.HashHex == "" || h.SaltHex == "" || h.Params.T == 0 || h.Params.M == 0 || h.Params.P == 0 {
		return nil, errors.New("argon2pw.Load: incomplete hash record")
	}
	return &h, nil
}

// Verify constant-time compares argon2id(plaintext, salt, params) to the
// stored hash. Returns nil if the user's plaintext is correct, an error
// otherwise. Caller must not leak the error to the network as anything
// more specific than "wrong password".
func (h *Hash) Verify(plaintext string) error {
	salt, err := hex.DecodeString(h.SaltHex)
	if err != nil {
		return fmt.Errorf("argon2pw.Verify: bad salt: %w", err)
	}
	want, err := hex.DecodeString(h.HashHex)
	if err != nil {
		return fmt.Errorf("argon2pw.Verify: bad hash: %w", err)
	}
	got := argon2.IDKey([]byte(plaintext), salt, h.Params.T, h.Params.M, h.Params.P, uint32(len(want)))
	if subtle.ConstantTimeCompare(got, want) != 1 {
		return errors.New("argon2pw.Verify: mismatch")
	}
	return nil
}
