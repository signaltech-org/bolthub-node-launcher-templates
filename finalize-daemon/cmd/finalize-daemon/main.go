// Command finalize-daemon is the bolthub one-time-token finalize daemon
// that runs on the user's VM. Cloud-init starts it as a systemd unit and
// Caddy proxies /.well-known/bolthub/* to it on loopback.
//
// The daemon's only purpose is to take BoltHub's server out of the
// finalize-loop: the browser drives finalize directly through this
// daemon, the daemon talks to the local litd over loopback to bake
// scoped macaroons + create an LNC session, and only then phones the
// scoped macaroons home. The plaintext LNC pairing phrase never leaves
// the user's browser.
package main

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/signaltech-org/bolthub-node-launcher-templates/finalize-daemon/internal/argon2pw"
	"github.com/signaltech-org/bolthub-node-launcher-templates/finalize-daemon/internal/handler"
	"github.com/signaltech-org/bolthub-node-launcher-templates/finalize-daemon/internal/lnd"
	"github.com/signaltech-org/bolthub-node-launcher-templates/finalize-daemon/internal/tokens"
)

func main() {
	if err := run(); err != nil {
		log.Fatalf("[finalize-daemon] fatal: %v", err)
	}
}

func run() error {
	cfg, err := loadConfig()
	if err != nil {
		return err
	}

	v, err := tokens.New([]byte(cfg.WebhookSecret), cfg.ConsumedTokensPath)
	if err != nil {
		return fmt.Errorf("tokens: %w", err)
	}

	lndClient, err := lnd.New(cfg.LndBaseURL, cfg.LndMacaroonPath)
	if err != nil {
		return fmt.Errorf("lnd: %w", err)
	}

	srv := handler.New(cfg.NodeID, cfg.CallbackURL, v, lndAdapter{lndClient})

	if cfg.LitdPasswordHashPath != "" {
		if hash, err := argon2pw.Load(cfg.LitdPasswordHashPath); err != nil {
			return fmt.Errorf("load litd password hash: %w", err)
		} else if hash != nil {
			log.Printf("[finalize-daemon] litd password hash loaded from %s", cfg.LitdPasswordHashPath)
			srv.PasswordHash = hash
		}
	}

	httpSrv := &http.Server{
		Addr:              cfg.Listen,
		Handler:           srv.Routes(),
		ReadHeaderTimeout: 10 * time.Second,
	}

	go func() {
		log.Printf("[finalize-daemon] listening on %s", cfg.Listen)
		if err := httpSrv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Printf("[finalize-daemon] listen error: %v", err)
		}
	}()

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	<-ctx.Done()

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	return httpSrv.Shutdown(shutdownCtx)
}

type config struct {
	NodeID               string
	WebhookSecret        string
	CallbackURL          string
	Listen               string
	LndBaseURL           string
	LndMacaroonPath      string
	ConsumedTokensPath   string
	LitdPasswordHashPath string
}

func loadConfig() (config, error) {
	c := config{
		NodeID:               os.Getenv("BOLTHUB_NODE_ID"),
		WebhookSecret:        os.Getenv("BOLTHUB_WEBHOOK_SECRET"),
		CallbackURL:          os.Getenv("BOLTHUB_CALLBACK_URL"),
		Listen:               envOr("BOLTHUB_LISTEN", "127.0.0.1:7681"),
		LndBaseURL:           envOr("BOLTHUB_LND_BASE_URL", "https://127.0.0.1:8443"),
		LndMacaroonPath:      envOr("BOLTHUB_LND_MACAROON_PATH", "/root/.lit/admin.macaroon"),
		ConsumedTokensPath:   envOr("BOLTHUB_CONSUMED_TOKENS_PATH", "/var/lib/bolthub/consumed-tokens.log"),
		LitdPasswordHashPath: os.Getenv("BOLTHUB_LITD_PASSWORD_HASH_PATH"),
	}
	if c.NodeID == "" {
		return c, fmt.Errorf("BOLTHUB_NODE_ID must be set")
	}
	if c.WebhookSecret == "" {
		return c, fmt.Errorf("BOLTHUB_WEBHOOK_SECRET must be set")
	}
	if c.CallbackURL == "" {
		return c, fmt.Errorf("BOLTHUB_CALLBACK_URL must be set")
	}
	return c, nil
}

func envOr(k, d string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return d
}

// lndAdapter bridges the lnd.Client API to the handler package's smaller
// interface (handler defines its own MacaroonPermission so it doesn't have
// to import lnd in tests).
type lndAdapter struct{ c *lnd.Client }

func (a lndAdapter) BakeMacaroon(ctx context.Context, perms []handler.MacaroonPermission) (string, error) {
	out := make([]lnd.MacaroonPermission, len(perms))
	for i, p := range perms {
		out[i] = lnd.MacaroonPermission{Entity: p.Entity, Action: p.Action}
	}
	return a.c.BakeMacaroon(ctx, out)
}

func (a lndAdapter) CreateLNCSession(ctx context.Context, expiry time.Time) (string, error) {
	return a.c.CreateLNCSession(ctx, expiry)
}

func (a lndAdapter) RestoreChannelBackups(ctx context.Context, blob string) error {
	return a.c.RestoreChannelBackups(ctx, blob)
}
