// Command apollo serves the enrollment, agreement and tuition-payment site, and runs the staff account CLI (apollo user ...) and the Sentry check (apollo sentry-check). See docs/architecture.md and docs/operations/startup-shutdown.md.
package main

import (
	"cmp"
	"context"
	"embed"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"github.com/stripe/stripe-go/v87"
	"gorm.io/gorm"

	"apollo/internal/auth"
	"apollo/internal/billing"
	"apollo/internal/customer"
	"apollo/internal/db"
	"apollo/internal/links"
	"apollo/internal/live"
	"apollo/internal/logging"
	"apollo/internal/logging/dblog"
	"apollo/internal/logging/httplog"
	"apollo/internal/logging/sentrylog"
	"apollo/internal/management"
	"apollo/internal/payments"
	"apollo/internal/settings"
	"apollo/internal/students"
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
		case "sentry-check":
		default:
			fmt.Fprintf(os.Stderr, "unknown command %q; use --help\n", args[0])
			os.Exit(1)
		}
	}

	out := os.Stdout
	if len(args) > 0 {
		out = os.Stderr
	}
	environment := cmp.Or(os.Getenv("RAILWAY_ENVIRONMENT_NAME"), "local")
	version := cmp.Or(os.Getenv("RAILWAY_GIT_COMMIT_SHA"), "unknown")
	reporter, sentryErr := sentrylog.New(sentrylog.Options{DSN: os.Getenv("SENTRY_DSN"), Environment: environment, Release: version})
	log := newLogger(out, reporter).With(
		"service", cmp.Or(os.Getenv("RAILWAY_SERVICE_NAME"), "Apollo"),
		"environment", environment,
		"version", version,
	)
	if id := os.Getenv("RAILWAY_DEPLOYMENT_ID"); id != "" {
		log = log.With("deployment_id", id)
	}
	if id := os.Getenv("RAILWAY_REPLICA_ID"); id != "" {
		log = log.With("replica_id", id)
	}
	var err error
	switch {
	case sentryErr != nil:
		err = startupError{"sentry_configuration", sentryErr}
	case len(args) > 0 && args[0] == "sentry-check":
		if err = checkSentry(log, reporter); err != nil {
			fmt.Fprintln(os.Stderr, err)
		}
	default:
		err = run(log, reporter, args)
	}
	var failed startupError
	if errors.As(err, &failed) {
		log.LogAttrs(context.Background(), slog.LevelError, "startup_failed",
			append(logging.ErrorAttrs(failed.err), slog.String("stage", failed.stage))...)
	}
	// Last, so startup_failed and the shutdown events reach Sentry too.
	reporter.Flush(2 * time.Second)
	if err != nil {
		os.Exit(1)
	}
}

// newLogger writes JSON events to out and, when Sentry is on, also reports error events to it.
func newLogger(out io.Writer, reporter *sentrylog.Reporter) *slog.Logger {
	log := logging.New(out)
	if reporter == nil {
		return log
	}
	return slog.New(slog.NewMultiHandler(log.Handler(), reporter.Handler()))
}

// checkSentry logs one synthetic sentry_check error, which carries no data, through the normal logger, waits
// for the SDK to send it, and prints its event ID. The SDK does not report Sentry's answer, so whether the
// event arrived is confirmed in Sentry by that ID.
func checkSentry(log *slog.Logger, reporter *sentrylog.Reporter) error {
	if reporter == nil {
		return errors.New("SENTRY_DSN is not set, so Sentry is off")
	}
	log.ErrorContext(logging.WithOperation(context.Background(), "sentry_check"), "sentry_check")
	if !reporter.Flush(2 * time.Second) {
		return fmt.Errorf("sentry_check event %s was still queued after 2s", reporter.LastEventID())
	}
	fmt.Printf("sentry_check event %s handed to Sentry; search for this ID in Sentry to confirm it arrived\n", reporter.LastEventID())
	return nil
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
		&links.Enrollment{},
		&students.Student{},
	}, billing.Models()...)
}

func run(log *slog.Logger, reporter *sentrylog.Reporter, args []string) error {
	if len(args) == 0 {
		log.Info("startup_started")
	}
	slow := dblog.New(log)
	defer slow.Flush()

	signalContext, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	dsn := os.Getenv("DATABASE_URL")
	if dsn == "" {
		return startupError{"configuration", errors.New("DATABASE_URL is required")}
	}
	database, err := db.Open(dsn, slow)
	if err != nil {
		return startupError{"database_open", err}
	}
	// Deferred before the workers' stop, so the pool closes after the server and the workers are done.
	defer database.Close()
	slow.WatchPool(database.Pool)

	ctx, cancel := context.WithTimeout(signalContext, 10*time.Second)
	defer cancel()
	if err := database.Pool.Ping(ctx); err != nil {
		return startupError{"database_connect", err}
	}
	err = database.GORM.WithContext(logging.WithOperation(ctx, "startup_migrate")).AutoMigrate(models()...)
	slow.Flush()
	if err != nil {
		return startupError{"database_migrate", err}
	}

	if len(args) > 0 {
		err := auth.RunCLI(logging.WithOperation(signalContext, "cli_user"), database.GORM, args[1:])
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
		}
		return err
	}

	// Committed writes tell open management pages to refresh themselves.
	feed := live.New()

	billingCfg, stripeAPIKey, err := billing.LoadConfigFromEnv()
	if err != nil {
		return startupError{"billing_configuration", err}
	}
	// Testing aid: installments due this far apart instead of monthly. Never on a live Stripe account.
	if value, ok := os.LookupEnv("INSTALLMENT_SPACING"); ok {
		spacing, err := time.ParseDuration(value)
		switch {
		case err != nil || spacing <= 0:
			return startupError{"configuration", errors.New("INSTALLMENT_SPACING must be a positive duration such as 15m")}
		case billingCfg.Live:
			return startupError{"configuration", errors.New("INSTALLMENT_SPACING is for sandbox testing and is refused with STRIPE_MODE=live")}
		}
		payments.InstallmentSpacing = spacing
	}
	stripeTimer := httplog.NewStripeTimer(log, http.DefaultTransport)
	defer stripeTimer.Flush()
	stripeClient := billing.NewStripeClient(stripeAPIKey, stripeTimer)
	catalogCtx, cancelCatalog := context.WithTimeout(logging.WithOperation(signalContext, "startup_catalog"), 15*time.Second)
	defer cancelCatalog()
	catalog, err := billing.LoadCatalog(catalogCtx, stripeClient, billingCfg.Live)
	if err != nil {
		return startupError{"billing_catalog", err}
	}
	// Railway sets PORT and routes to every interface; without it the server stays on loopback.
	addr := "127.0.0.1:3000"
	if port := os.Getenv("PORT"); port != "" {
		addr = ":" + port
	}
	app, err := newApp(addr, database.GORM, database.GORM, feed, stripeClient, billingCfg, catalog, log, reporter)
	if err != nil {
		return err
	}

	workerCtx, cancelWorker := context.WithCancel(signalContext)
	var worker sync.WaitGroup
	worker.Go(func() { app.biller.Run(workerCtx) })
	worker.Go(func() { app.authentication.Run(workerCtx) })
	defer func() {
		cancelWorker()
		worker.Wait()
	}()

	listener, err := net.Listen("tcp", app.server.Addr)
	if err != nil {
		return startupError{"listen", err}
	}
	log.Info("server_listening", "addr", listener.Addr().String())
	return serve(signalContext, app.server, listener, log, func() { slow.Flush(); stripeTimer.Flush() }, 10*time.Second)
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

// application is the HTTP server plus the two services whose workers run starts beside it.
type application struct {
	server         *http.Server
	biller         *billing.Service
	authentication *auth.Manager
}

// newApp is the one place run and the wiring tests build billing, staff auth and the routes, so
// the tests exercise the real composition. Auth and billing use db; newServer's stores use served
// (run passes db twice; tests may hand over a broken connection).
func newApp(addr string, db, served *gorm.DB, feed *live.Feed, stripeClient *stripe.Client, cfg billing.Config, catalog billing.Catalog, log *slog.Logger, reporter *sentrylog.Reporter) (application, error) {
	biller, err := billing.New(db, stripeClient, cfg, catalog, feed.Changed, log)
	if err != nil {
		return application{}, startupError{"billing_initialize", err}
	}
	// The public origin alone decides cookie security: only a sandbox localhost origin may be plain http.
	secureCookies := cfg.SecureOrigin()
	authentication, err := auth.New(db, secureCookies, log)
	if err != nil {
		return application{}, startupError{"auth_initialize", err}
	}
	server, err := newServer(addr, served, feed, authentication, biller, secureCookies, log, reporter)
	if err != nil {
		return application{}, err
	}
	return application{server: server, biller: biller, authentication: authentication}, nil
}

func newServer(addr string, db *gorm.DB, feed *live.Feed, authentication *auth.Manager, biller *billing.Service, secureCookies bool, log *slog.Logger, reporter *sentrylog.Reporter) (*http.Server, error) {
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
	management.RegisterRoutes(managementMux, store, roster, config, auth.Staff, biller)
	mux.Handle("/management/", authentication.Require(httplog.Routes(managementMux)))
	if err := customer.RegisterRoutes(mux, store, roster, config, biller, secureCookies); err != nil {
		return nil, startupError{"customer_routes", err}
	}
	mux.Handle("POST /webhooks/stripe", http.HandlerFunc(biller.Webhook))

	server := &http.Server{
		Addr:              addr,
		Handler:           httplog.Requests(log, reporter.Middleware(secureHeaders(http.NewCrossOriginProtection().Handler(httplog.Routes(mux))))),
		ErrorLog:          httplog.ServerErrorLog(log),
		ReadHeaderTimeout: 5 * time.Second,
		WriteTimeout:      10 * time.Second,
		IdleTimeout:       60 * time.Second,
	}
	// Event streams never finish on their own; end them once shutdown starts.
	server.RegisterOnShutdown(feed.Close)
	return server, nil
}

func serve(ctx context.Context, server *http.Server, listener net.Listener, log *slog.Logger, flush func(), drain time.Duration) error {
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
			flush()
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
