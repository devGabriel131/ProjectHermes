// Command projecthermes serves the enrollment, agreement and tuition-payment site, and runs the staff account CLI (projecthermes user ...). See docs/architecture.md and docs/operations/startup-shutdown.md.
package main

import (
	"context"
	"embed"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"sync"
	"syscall"
	"time"

	"gorm.io/driver/postgres"
	"gorm.io/gorm"

	"projecthermes/internal/auth"
	"projecthermes/internal/billing"
	"projecthermes/internal/customer"
	"projecthermes/internal/links"
	"projecthermes/internal/live"
	"projecthermes/internal/logging"
	"projecthermes/internal/management"
	"projecthermes/internal/settings"
	"projecthermes/internal/students"
)

//go:embed static
var assets embed.FS

func main() {
	args := os.Args[1:]
	if len(args) > 0 {
		switch args[0] {
		case "--help", "-h":
			fmt.Print(auth.Usage)
			return
		case "user":
			if len(args) == 1 || len(args) == 2 && (args[1] == "--help" || args[1] == "-h") {
				fmt.Print(auth.Usage)
				return
			}
		default:
			fmt.Fprintf(os.Stderr, "unknown command %q; use --help\n", args[0])
			os.Exit(1)
		}
	}

	out := os.Stdout
	if len(args) > 0 {
		out = os.Stderr
	}
	service := os.Getenv("RAILWAY_SERVICE_NAME")
	if service == "" {
		service = "ProjectHermes"
	}
	environment := os.Getenv("RAILWAY_ENVIRONMENT_NAME")
	if environment == "" {
		environment = "local"
	}
	version := os.Getenv("RAILWAY_GIT_COMMIT_SHA")
	if version == "" {
		version = "unknown"
	}
	log := logging.New(out).With("service", service, "environment", environment, "version", version)
	if id := os.Getenv("RAILWAY_DEPLOYMENT_ID"); id != "" {
		log = log.With("deployment_id", id)
	}
	if id := os.Getenv("RAILWAY_REPLICA_ID"); id != "" {
		log = log.With("replica_id", id)
	}
	err := run(log, args)
	var failed startupError
	if errors.As(err, &failed) {
		log.LogAttrs(context.Background(), slog.LevelError, "startup_failed",
			append(logging.ErrorAttrs(failed.err), slog.String("stage", failed.stage))...)
	}
	if err != nil {
		os.Exit(1)
	}
}

// startupError marks a failure that main logs as startup_failed.
type startupError struct {
	stage string
	err   error
}

func (e startupError) Error() string { return e.stage + ": " + e.err.Error() }

func models() []any {
	return append([]any{
		&auth.User{},
		&auth.Session{},
		&settings.Settings{},
		&links.Link{},
		&students.Student{},
	}, billing.Models()...)
}

func run(log *slog.Logger, args []string) error {
	if len(args) == 0 {
		log.Info("startup_started")
	}
	slow := logging.NewDBLogger(log)
	defer slow.Flush()

	signalContext, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	dsn := os.Getenv("DATABASE_URL")
	if dsn == "" {
		return startupError{"configuration", errors.New("DATABASE_URL is required")}
	}
	db, err := gorm.Open(postgres.Open(dsn), &gorm.Config{
		DisableAutomaticPing:   true,
		SkipDefaultTransaction: true,
		Logger:                 slow,
	})
	if err != nil {
		return startupError{"database_open", err}
	}
	sqlDB, err := db.DB()
	if err != nil {
		return startupError{"database_pool", err}
	}
	defer sqlDB.Close()
	sqlDB.SetMaxOpenConns(10)
	sqlDB.SetMaxIdleConns(10)

	ctx, cancel := context.WithTimeout(signalContext, 10*time.Second)
	defer cancel()
	if err := sqlDB.PingContext(ctx); err != nil {
		return startupError{"database_connect", err}
	}
	err = db.WithContext(logging.WithOperation(ctx, "startup_migrate")).AutoMigrate(models()...)
	slow.Flush()
	if err != nil {
		return startupError{"database_migrate", err}
	}

	if len(args) > 0 {
		err := auth.RunCLI(logging.WithOperation(signalContext, "cli_user"), db, args[1:])
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
		}
		return err
	}

	secureCookies := true
	if value, ok := os.LookupEnv("SESSION_COOKIE_SECURE"); ok {
		secureCookies, err = strconv.ParseBool(value)
		if err != nil {
			return startupError{"configuration", err}
		}
	}
	// Committed writes tell open management pages to refresh themselves.
	feed := live.New()

	billingCfg, stripeAPIKey, err := billing.LoadConfigFromEnv()
	if err != nil {
		return startupError{"billing_configuration", err}
	}
	stripeClient := billing.NewStripeClient(stripeAPIKey)
	catalogCtx, cancelCatalog := context.WithTimeout(signalContext, 15*time.Second)
	defer cancelCatalog()
	catalog, err := billing.LoadCatalog(catalogCtx, stripeClient, billingCfg.Live)
	if err != nil {
		return startupError{"billing_catalog", err}
	}
	biller, err := billing.New(db, stripeClient, billingCfg, catalog, feed.Changed, log)
	if err != nil {
		return startupError{"billing_initialize", err}
	}

	authentication, err := auth.New(db, secureCookies, log)
	if err != nil {
		return startupError{"auth_initialize", err}
	}
	defer authentication.Close()

	addr := "127.0.0.1:3000"
	if port := os.Getenv("PORT"); port != "" {
		addr = ":" + port
	}
	server, err := newServer(addr, db, feed, authentication, biller, log)
	if err != nil {
		return err
	}

	workerCtx, cancelWorker := context.WithCancel(signalContext)
	var worker sync.WaitGroup
	worker.Go(func() { biller.Run(workerCtx) })
	defer func() {
		cancelWorker()
		worker.Wait()
	}()

	listener, err := net.Listen("tcp", server.Addr)
	if err != nil {
		return startupError{"listen", err}
	}
	log.Info("server_listening", "addr", listener.Addr().String())
	return serve(signalContext, server, listener, log, slow, 10*time.Second)
}

// sitePolicy is the Content-Security-Policy of every response. Pages load local assets, the one
// pinned htmx file and Google Fonts; forms post here or to hosted Checkout. An htmx upgrade
// changes the script URL in the templates and here together.
const sitePolicy = "default-src 'self'; script-src 'self' https://cdn.jsdelivr.net/npm/htmx.org@2.0.11/dist/htmx.min.js; " +
	"style-src 'self' https://fonts.googleapis.com; font-src https://fonts.gstatic.com; img-src 'self' data:; connect-src 'self'; " +
	"frame-ancestors 'none'; object-src 'none'; base-uri 'self'; form-action 'self' https://checkout.stripe.com;"

// secureHeaders sets the browser policies every response carries, whichever handler answers.
func secureHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		header := w.Header()
		header.Set("Content-Security-Policy", sitePolicy)
		header.Set("Strict-Transport-Security", "max-age=31536000")
		header.Set("X-Content-Type-Options", "nosniff")
		header.Set("X-Robots-Tag", "noindex, nofollow")
		next.ServeHTTP(w, r)
	})
}

func newServer(addr string, db *gorm.DB, feed *live.Feed, authentication *auth.Manager, biller *billing.Service, log *slog.Logger) (*http.Server, error) {
	store := links.New(db, feed.Changed)
	roster := students.New(db, feed.Changed)
	config := settings.New(db)

	mux := http.NewServeMux()
	mux.Handle("GET /static/", http.FileServerFS(assets))
	mux.Handle("GET /static/{$}", http.NotFoundHandler())
	mux.HandleFunc("GET /favicon.ico", func(w http.ResponseWriter, r *http.Request) {
		http.ServeFileFS(w, r, assets, "static/favicon.ico")
	})
	mux.HandleFunc("GET /{$}", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/management/", http.StatusFound)
	})
	authentication.RegisterRoutes(mux)
	managementMux := http.NewServeMux()
	managementMux.Handle("GET /management/events", feed)
	if err := management.RegisterRoutes(managementMux, store, roster, config, auth.Staff, biller); err != nil {
		return nil, startupError{"management_routes", err}
	}
	mux.Handle("/management/", authentication.Require(logging.Routes(managementMux)))
	if err := customer.RegisterRoutes(mux, store, roster, config, biller); err != nil {
		return nil, startupError{"customer_routes", err}
	}
	mux.Handle("POST /webhooks/stripe", http.HandlerFunc(biller.Webhook))

	server := &http.Server{
		Addr:              addr,
		Handler:           logging.Requests(log, secureHeaders(http.NewCrossOriginProtection().Handler(logging.Routes(mux)))),
		ErrorLog:          logging.ServerErrorLog(log),
		ReadHeaderTimeout: 5 * time.Second,
		WriteTimeout:      10 * time.Second,
		IdleTimeout:       60 * time.Second,
	}
	// Event streams never finish on their own; end them once shutdown starts.
	server.RegisterOnShutdown(feed.Close)
	return server, nil
}

func serve(ctx context.Context, server *http.Server, listener net.Listener, log *slog.Logger, slow *logging.DBLogger, drain time.Duration) error {
	result := make(chan error, 1)
	go func() { result <- server.Serve(listener) }()
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case err := <-result:
			if errors.Is(err, http.ErrServerClosed) {
				return nil
			}
			log.LogAttrs(context.Background(), slog.LevelError, "server_failed",
				append(logging.ErrorAttrs(err), slog.String("stage", "serve"))...)
			return err
		case <-ticker.C:
			slow.Flush()
		case <-ctx.Done():
			started := time.Now()
			log.Info("shutdown_requested", "drain_timeout_ms", drain.Milliseconds())
			shutdownContext, cancel := context.WithTimeout(context.Background(), drain)
			defer cancel()
			err := server.Shutdown(shutdownContext)
			if err != nil {
				err = errors.Join(err, server.Close())
			}
			servingErr := <-result
			if errors.Is(servingErr, http.ErrServerClosed) {
				servingErr = nil
			}
			err = errors.Join(err, servingErr)
			duration := slog.Float64("duration_ms", float64(time.Since(started))/float64(time.Millisecond))
			if err != nil {
				log.LogAttrs(context.Background(), slog.LevelError, "shutdown_failed",
					append(logging.ErrorAttrs(err), duration)...)
				return err
			}
			log.LogAttrs(context.Background(), slog.LevelInfo, "shutdown_completed", duration)
			return nil
		}
	}
}
