package auth

import (
	"context"
	"crypto/rand"
	"embed"
	"errors"
	"html/template"
	"log/slog"
	"net/http"
	"strconv"
	"time"

	"github.com/alexedwards/argon2id"
	"github.com/alexedwards/scs/postgresstore"
	"github.com/alexedwards/scs/v2"
	"golang.org/x/time/rate"
	"gorm.io/gorm"

	"projecthermes/internal/logging"
)

//go:embed templates/login.html
var templates embed.FS

const (
	cookieName         = "management_session"
	usernameKey        = "username"
	versionKey         = "auth_version"
	loginInterval      = 12 * time.Second
	invalidCredentials = "Invalid username or password."
)

type loginPage struct {
	Username string
	Error    string
}

type Manager struct {
	db        *gorm.DB
	sessions  *scs.SessionManager
	page      *template.Template
	dummyHash string
	// ponytail: one limit shared by every client bounds guessing and Argon2's
	// 64 MiB per check. Accepted risk: an outsider who keeps it spent blocks new
	// staff sign-ins; key it per client IP if that ever happens.
	limiter *rate.Limiter
	log     *slog.Logger
	// throttled keeps a guessing run from flooding the log with one line per refusal.
	throttled rate.Sometimes
}

// New owns staff-session configuration, not the SQL pool; Run deletes expired sessions.
func New(db *gorm.DB, secureCookies bool, log *slog.Logger) (*Manager, error) {
	page, err := template.ParseFS(templates, "templates/login.html")
	if err != nil {
		return nil, err
	}
	// Unknown usernames are verified against this hash so they cost the same as wrong passwords.
	dummyHash, err := argon2id.CreateHash(rand.Text(), argon2id.DefaultParams)
	if err != nil {
		return nil, err
	}
	sqlDB, err := db.DB()
	if err != nil {
		return nil, err
	}
	sessions := scs.New()
	sessions.Store = postgresstore.NewWithCleanupInterval(sqlDB, 0)
	sessions.HashTokenInStore = true
	sessions.Lifetime = 14 * 24 * time.Hour
	sessions.Cookie.Name = cookieName
	sessions.Cookie.Path = "/management/"
	sessions.Cookie.HttpOnly = true
	sessions.Cookie.SameSite = http.SameSiteLaxMode
	sessions.Cookie.Persist = true
	sessions.Cookie.Secure = secureCookies
	sessions.ErrorFunc = func(w http.ResponseWriter, r *http.Request, err error) {
		logging.RecordError(r, "session", err)
		http.Error(w, http.StatusText(http.StatusInternalServerError), http.StatusInternalServerError)
	}
	return &Manager{
		db:        db,
		sessions:  sessions,
		page:      page,
		dummyHash: dummyHash,
		limiter:   rate.NewLimiter(rate.Every(loginInterval), 2),
		log:       log,
		throttled: rate.Sometimes{Interval: time.Minute},
	}, nil
}

// Run deletes expired sessions every 5 minutes until ctx ends.
func (m *Manager) Run(ctx context.Context) {
	ticker := time.NewTicker(5 * time.Minute)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			m.deleteExpiredSessions(ctx)
		}
	}
}

func (m *Manager) deleteExpiredSessions(ctx context.Context) {
	cleanupCtx, cancel := context.WithTimeout(ctx, dbTimeout)
	defer cancel()
	err := m.db.WithContext(logging.WithOperation(cleanupCtx, "session_cleanup")).
		Where("expiry < CURRENT_TIMESTAMP").Delete(&Session{}).Error
	if err != nil && ctx.Err() == nil {
		m.log.LogAttrs(ctx, slog.LevelError, "session_cleanup_failed", logging.ErrorAttrs(err)...)
	}
}

func (m *Manager) RegisterRoutes(mux *http.ServeMux) {
	mux.Handle("POST /management/login", m.sessions.LoadAndSave(http.HandlerFunc(m.login)))
	mux.Handle("POST /management/logout", m.sessions.LoadAndSave(http.HandlerFunc(m.logout)))
}

type staffKey struct{}

// Staff returns the username Require verified for this request, or "" for a
// request that didn't pass through Require.
func Staff(ctx context.Context) string {
	username, _ := ctx.Value(staffKey{}).(string)
	return username
}

// Require serves next only for a session whose account still exists with the
// auth version recorded at sign-in, exposing the account through Staff.
func (m *Manager) Require(next http.Handler) http.Handler {
	return m.sessions.LoadAndSave(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		username, err := m.authenticated(r.Context())
		if err != nil {
			logging.RecordError(r, "authenticate", err)
			http.Error(w, "Could not check your sign-in. Please try again.", http.StatusInternalServerError)
			return
		}
		switch {
		case username != "":
			next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), staffKey{}, username)))
		case r.Header.Get("HX-Request") == "true":
			// A swapped-in sign-in form would replace the link list, so send htmx to the full page.
			w.Header().Set("HX-Redirect", "/management/")
			http.Error(w, "Sign in required.", http.StatusUnauthorized)
		case (r.Method == http.MethodGet || r.Method == http.MethodHead) && r.URL.Path == "/management/":
			// 200 keeps the Railway health check on /management/ passing while signed out.
			m.render(w, r, http.StatusOK, loginPage{})
		default:
			http.Redirect(w, r, "/management/", http.StatusSeeOther)
		}
	}))
}

// authenticated returns the session's username, or "" when it is signed out.
func (m *Manager) authenticated(ctx context.Context) (string, error) {
	username := m.sessions.GetString(ctx, usernameKey)
	if username == "" {
		return "", nil
	}
	user, err := findUser(ctx, m.db, username)
	if err != nil {
		return "", err
	}
	if user != nil && user.AuthVersion == m.sessions.GetString(ctx, versionKey) {
		return user.Username, nil
	}
	return "", m.sessions.Destroy(ctx)
}

// logout ends only this browser's session; other devices stay signed in.
// Signed-out requests have nothing to end and are redirected the same way.
func (m *Manager) logout(w http.ResponseWriter, r *http.Request) {
	if err := m.sessions.Destroy(r.Context()); err != nil {
		logging.RecordError(r, "logout", err)
		http.Error(w, "Could not sign out. Please try again.", http.StatusInternalServerError)
		return
	}
	http.Redirect(w, r, "/management/", http.StatusSeeOther)
}

func (m *Manager) login(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, 4<<10)
	if err := r.ParseForm(); err != nil {
		status := http.StatusBadRequest
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			status = http.StatusRequestEntityTooLarge
		}
		m.render(w, r, status, loginPage{Error: "Could not read your sign-in. Please try again."})
		return
	}
	page := loginPage{Username: r.PostForm.Get("username")}
	if !m.limiter.Allow() {
		m.throttled.Do(func() { m.log.LogAttrs(r.Context(), slog.LevelWarn, "staff_login_throttled") })
		w.Header().Set("Retry-After", strconv.Itoa(int(loginInterval/time.Second)))
		page.Error = "Too many sign-in attempts. Please wait a few seconds and try again."
		m.render(w, r, http.StatusTooManyRequests, page)
		return
	}

	ctx := r.Context()
	user, err := m.verify(ctx, page.Username, r.PostForm.Get("password"))
	if err == nil && user != nil {
		// A fresh token prevents session fixation.
		err = m.sessions.RenewToken(ctx)
	}
	if err != nil {
		logging.RecordError(r, "login", err)
		page.Error = "Could not sign in. Please try again."
		m.render(w, r, http.StatusInternalServerError, page)
		return
	}
	if user == nil {
		m.log.LogAttrs(ctx, slog.LevelWarn, "staff_login_failed")
		page.Error = invalidCredentials
		m.render(w, r, http.StatusUnauthorized, page)
		return
	}
	m.sessions.Put(ctx, usernameKey, user.Username)
	m.sessions.Put(ctx, versionKey, user.AuthVersion)
	http.Redirect(w, r, "/management/", http.StatusSeeOther)
}

// verify returns nil without an error for unknown usernames and wrong passwords.
func (m *Manager) verify(ctx context.Context, submittedUsername, password string) (*User, error) {
	var user *User
	if username, ok := normalizeUsername(submittedUsername); ok {
		var err error
		if user, err = findUser(ctx, m.db, username); err != nil {
			return nil, err
		}
	}
	hash := m.dummyHash
	if user != nil {
		hash = user.PasswordHash
	}
	match, err := argon2id.ComparePasswordAndHash(password, hash)
	if err != nil || !match {
		return nil, err
	}
	return user, nil
}

func (m *Manager) render(w http.ResponseWriter, r *http.Request, status int, page loginPage) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Referrer-Policy", "no-referrer")
	w.WriteHeader(status)
	if err := m.page.Execute(w, page); err != nil {
		logging.RecordError(r, "render", err)
	}
}
