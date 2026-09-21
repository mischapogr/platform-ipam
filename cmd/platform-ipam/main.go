package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"

	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/mischapogr/platform-ipam/internal/adoptcmd"
	"github.com/mischapogr/platform-ipam/internal/cli"
	"github.com/mischapogr/platform-ipam/internal/cloud"
	"github.com/mischapogr/platform-ipam/internal/config"
	"github.com/mischapogr/platform-ipam/internal/domain"
	"github.com/mischapogr/platform-ipam/internal/netbox"
	"github.com/mischapogr/platform-ipam/internal/onboardcmd"
	"github.com/mischapogr/platform-ipam/internal/service"
	"github.com/mischapogr/platform-ipam/internal/storage"
	"github.com/mischapogr/platform-ipam/internal/transport"
)

func main() {
	slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stdout, nil)))
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	if err := run(ctx, os.Args[1:]); err != nil {
		slog.Error("process stopped", "reason", err.Error())
		os.Exit(1)
	}
}
func run(ctx context.Context, args []string) error {
	// The consumer client (ADR 0003) ships in this binary but shares nothing
	// with the server modes: it needs no database, no configuration file, and
	// no identity mapping, so it returns before any of that is loaded.
	if len(args) > 0 && args[0] == "client" {
		if code := cli.Main(ctx, args[1:], os.Stdout, os.Stderr); code != 0 {
			os.Exit(code)
		}
		return nil
	}
	// The onboarding import process mode (ADR 0007, docs/ONBOARDING_IMPORT.md
	// section 2) is an operator tool, not a server mode: it needs the pools
	// configuration and, for apply, NetBox credentials, but it never opens
	// the ledger database, so -- like "client" -- it returns before
	// storage.NewPostgresLedger is even called.
	if len(args) > 0 && args[0] == "onboard" {
		if code := onboardcmd.Main(ctx, args[1:], os.Stdout, os.Stderr); code != 0 {
			os.Exit(code)
		}
		return nil
	}
	// "adopt" (ADR 0010, docs/WORK_PLAN.md package F4) is a sibling of
	// "onboard" that, unlike onboard, opens the ledger and needs the cloud
	// observer -- so its real work dispatches below, alongside api and
	// worker, sharing their settings/config/ledger/inventory construction
	// rather than a second copy of it. Its own usage validation happens here
	// instead, before any of that is built: a plain mistake ("adopt" with no
	// subcommand, an unknown subcommand, "apply" with no --operator) must be
	// reported without needing a working database and NetBox configured, the
	// same guarantee "client" and "onboard" already give.
	if len(args) > 0 && args[0] == "adopt" {
		if code, ok := adoptcmd.CheckArgs(args[1:], os.Stdout, os.Stderr); !ok {
			os.Exit(code)
			return nil
		}
	}
	if len(args) == 0 || (args[0] != "api" && args[0] != "worker" && args[0] != "migrate" && args[0] != "adopt") {
		return fmt.Errorf("usage: platform-ipam api|worker|migrate|client|onboard|adopt")
	}
	if args[0] != "adopt" && len(args) != 1 {
		return fmt.Errorf("usage: platform-ipam api|worker|migrate|client|onboard|adopt")
	}
	mode := args[0]
	settings := config.Environment()
	if err := settings.Validate(mode); err != nil {
		return err
	}
	ledger, err := storage.NewPostgresLedger(ctx, settings.DatabaseURL)
	if err != nil {
		return fmt.Errorf("database connection failed (verify endpoint, credentials and TLS)")
	}
	defer ledger.Close()
	if mode == "migrate" {
		if err := ledger.Migrate(ctx); err != nil {
			return fmt.Errorf("database migration failed")
		}
		slog.Info("database migration complete")
		return nil
	}
	if err := ledger.Ready(ctx); err != nil {
		return fmt.Errorf("ledger schema is not ready; run migrate before starting processes")
	}
	cfg, err := config.Load(settings.ConfigFile, settings.IdentityFile, settings.Environment)
	if err != nil {
		return err
	}
	// ADR 0011 stage one: configuration and identities are separate files and
	// roll independently, so an identity may legitimately be onboarded before
	// its pool exists. That is a loud warning, never a load failure -- the
	// honest refusal lives at GET /v1/pools instead (internal/transport/http.go).
	for _, id := range config.IdentitiesWithoutPool(cfg) {
		slog.Warn("identity's tenant is eligible for no pool", "subject", id.Subject, "tenant_id", id.TenantID)
	}
	if settings.UIInventoryLinks != "" {
		cfg.UI.InventoryLinksEnabled, _ = strconv.ParseBool(settings.UIInventoryLinks)
	}
	if settings.UINetBoxURL != "" {
		cfg.UI.NetBoxBaseURL = settings.UINetBoxURL
	}
	if cfg.UI.InventoryLinksEnabled && cfg.UI.NetBoxBaseURL == "" {
		cfg.UI.NetBoxBaseURL = settings.NetBoxURL
	}
	inventory, err := netbox.New(netbox.Config{BaseURL: settings.NetBoxURL, Token: settings.NetBoxToken, Domains: cfg.Domains, Pools: cfg.Pools})
	if err != nil {
		return fmt.Errorf("invalid inventory adapter configuration")
	}
	var observer domain.Observer
	if mode == "worker" || mode == "adopt" {
		// adopt gets a live observer of its own, exactly as worker does,
		// rather than relying only on whatever the worker last stored: it is
		// a manual, one-shot, irreversible action (ADR 0010, "no undo"), so
		// it asks the cloud directly for the freshest observation available
		// rather than trusting how recently a concurrently running worker
		// happened to tick.
		if settings.AWSMode == "fake" {
			observer = cloud.NewFake(cloud.FakeConfig{FixturePath: settings.FakeCloudFile})
		} else {
			awsCfg, err := awsconfig.LoadDefaultConfig(ctx)
			if err != nil {
				return fmt.Errorf("AWS credential configuration failed")
			}
			observer = cloud.NewAWS(cloud.Config{AWS: awsCfg})
		}
	}
	app := service.New(cfg, ledger, inventory, observer)
	if mode == "adopt" {
		// adopt never starts the HTTP server or the worker loop and never
		// migrates: it runs its subcommand and exits, exactly like client
		// and onboard above, except that -- needing the ledger and the
		// observer -- it dispatches here rather than before the database is
		// opened.
		if code := adoptcmd.Main(ctx, args[1:], cfg, app, os.Stdout, os.Stderr); code != 0 {
			os.Exit(code)
		}
		return nil
	}
	var handler http.Handler
	if mode == "api" {
		auth, err := transport.NewAuth(ctx, settings, cfg.Identities)
		if err != nil {
			return fmt.Errorf("API identity setup failed: %w", err)
		}
		handler = transport.New(app, auth, ledger, cfg).Handler()
	} else {
		mux := http.NewServeMux()
		mux.HandleFunc("GET /livez", func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(200) })
		mux.HandleFunc("GET /readyz", func(w http.ResponseWriter, r *http.Request) {
			if ledger.Ready(r.Context()) != nil {
				w.WriteHeader(503)
				return
			}
			w.WriteHeader(200)
		})
		handler = mux
	}
	server := &http.Server{Addr: settings.ListenAddr, Handler: handler, ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 35 * time.Second, WriteTimeout: 60 * time.Second, IdleTimeout: 60 * time.Second, MaxHeaderBytes: 32 << 10}
	failures := make(chan error, 1)
	go func() { failures <- server.ListenAndServe() }()
	workerCtx, stopWorker := context.WithCancel(ctx)
	defer stopWorker()
	done := make(chan struct{})
	if mode == "worker" {
		go func() {
			defer close(done)
			tick := func() {
				if err := app.Tick(workerCtx); err != nil && workerCtx.Err() == nil {
					slog.Warn("worker pass incomplete; holds retained", "error_type", fmt.Sprintf("%T", err))
				}
			}
			tick()
			timer := time.NewTicker(time.Duration(cfg.Reconciliation.FullScanInterval) * time.Second)
			defer timer.Stop()
			for {
				select {
				case <-workerCtx.Done():
					return
				case <-timer.C:
					tick()
				}
			}
		}()
	} else {
		close(done)
	}
	slog.Info("process ready", "mode", mode, "environment", settings.Environment, "policy_version", cfg.PolicyVersion)
	select {
	case err := <-failures:
		stopWorker()
		if !errors.Is(err, http.ErrServerClosed) {
			return fmt.Errorf("HTTP listener failed: %w", err)
		}
	case <-ctx.Done():
	}
	stopWorker()
	shutdown, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	err = server.Shutdown(shutdown)
	select {
	case <-done:
	case <-shutdown.Done():
	}
	return err
}
