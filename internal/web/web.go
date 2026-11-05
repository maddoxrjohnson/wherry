// Package web is the dashboard: a few server-rendered pages that show what
// the API knows and stream live logs into the browser. They are on the
// internal listener and, with a dashboard host configured, public through
// the router. Login is the admin token; a cookie carries the session after
// that. There is no framework and no build step: html/template, embed, and
// a few dozen lines of JavaScript for the log view and the reveal buttons.
package web

import (
	"context"
	"embed"
	"errors"
	"html/template"
	"io/fs"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"github.com/maddoxrjohnson/wherry/internal/gitcmd"
	"github.com/maddoxrjohnson/wherry/internal/logs"
	"github.com/maddoxrjohnson/wherry/internal/ratelimit"
	"github.com/maddoxrjohnson/wherry/internal/runtime"
	"github.com/maddoxrjohnson/wherry/internal/site"
	"github.com/maddoxrjohnson/wherry/internal/store"
	"github.com/maddoxrjohnson/wherry/internal/supervisor"
)

// Store is the slice of the database the pages read.
type Store interface {
	GetApp(ctx context.Context, name string) (store.App, error)
	ListApps(ctx context.Context) ([]store.App, error)
	ListReleases(ctx context.Context, appID int64) ([]store.Release, error)
	LiveRelease(ctx context.Context, appID int64) (store.Release, error)
	GetConfig(ctx context.Context, appID int64) (map[string]string, error)
}

// Supervisor reports instances.
type Supervisor interface {
	Status(app string) (supervisor.Status, bool)
}

// Logs is the live log source; *logs.Hub satisfies it.
type Logs interface {
	Subscribe(app string, tail int) (<-chan logs.Line, func())
}

// Config is what the pages need to know about the daemon.
type Config struct {
	DataDir string    // where the builder keeps build logs
	Site    site.Site // how apps are named and linked
	Version string
}

const (
	cookieName = "wherry_session"
	sessionTTL = 24 * time.Hour
	logTail    = 100

	// A login guesses at the admin token, so it is limited far below the
	// router's request rate: a burst for typos, then one try per refill.
	loginBurst  = 5
	loginRefill = 30 * time.Second

	maxLoginBody = 4 << 10 // a token and a little form encoding

	// Everything the pages load is same-origin and none of it is inline.
	contentSecurityPolicy = "default-src 'self'; base-uri 'none'; form-action 'self'; frame-ancestors 'none'"

	// strictTransportSecurity leaves out includeSubDomains: the apps under
	// this name are their own business.
	strictTransportSecurity = "max-age=31536000"

	// sessionCheck is how often a log stream looks at its session. The
	// session is only checked when a request starts, and a stream is one
	// request that can outlive it by days.
	sessionCheck = 30 * time.Second
)

//go:embed templates/*.html static/*
var content embed.FS

// Server serves the dashboard. Create it with New.
type Server struct {
	store Store
	sup   Supervisor
	logs  Logs
	token []byte
	cfg   Config
	log   *slog.Logger
	mux   *http.ServeMux
	root  http.Handler // mux behind the cross-origin check
	pages map[string]*template.Template

	logins       *ratelimit.Limiter // attempts per client
	sessionCheck time.Duration

	mu       sync.Mutex
	sessions map[string]time.Time // id -> expiry
}

// New wires the pages. token is the admin token the login form accepts.
func New(st Store, sup Supervisor, lg Logs, token string, cfg Config, log *slog.Logger) *Server {
	if log == nil {
		log = slog.Default()
	}
	s := &Server{store: st, sup: sup, logs: lg, token: []byte(token), cfg: cfg, log: log,
		mux: http.NewServeMux(), pages: make(map[string]*template.Template), sessions: make(map[string]time.Time),
		logins: ratelimit.New(1/loginRefill.Seconds(), loginBurst), sessionCheck: sessionCheck}
	// In front of the mux, so a cross-origin form is refused before it can
	// spend a login attempt or log the operator out. It compares Origin
	// with the Host the browser sent, which a tunnel to the internal
	// listener leaves alone, and lets curl through, which sends neither.
	s.root = http.NewCrossOriginProtection().Handler(s.mux)
	for _, page := range []string{"login", "apps", "app"} {
		s.pages[page] = template.Must(template.New("").Funcs(template.FuncMap{
			"local":  func(t time.Time) string { return t.Local().Format(time.DateTime) },
			"short":  runtime.Short,
			"commit": gitcmd.ShortSHA,
			"since":  since,
		}).ParseFS(content, "templates/layout.html", "templates/"+page+".html"))
	}
	static, _ := fs.Sub(content, "static")
	s.mux.Handle("GET /static/", http.StripPrefix("/static/", http.FileServerFS(static)))
	s.mux.HandleFunc("GET /login", s.loginPage)
	s.mux.HandleFunc("POST /login", s.login)
	s.mux.HandleFunc("POST /logout", s.logout)
	s.mux.HandleFunc("GET /{$}", s.withSession(s.apps))
	s.mux.HandleFunc("GET /apps/{app}", s.withSession(s.app))
	s.mux.HandleFunc("GET /apps/{app}/logs", s.withSession(s.appLogs))
	s.mux.HandleFunc("GET /apps/{app}/builds/{sha}", s.withSession(s.buildLog))
	return s
}

// ServeHTTP routes to the pages; the session check is per route, since
// the login page and the static files have none. The headers go on every
// response, errors included, now that the pages can be public.
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	h := w.Header()
	h.Set("X-Frame-Options", "DENY")
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("Referrer-Policy", "same-origin")
	h.Set("Content-Security-Policy", contentSecurityPolicy)
	if r.TLS != nil {
		h.Set("Strict-Transport-Security", strictTransportSecurity)
	}
	s.root.ServeHTTP(w, r)
}

// render executes a page inside the layout.
func (s *Server) render(w http.ResponseWriter, status int, page string, data map[string]any) {
	if data == nil {
		data = map[string]any{}
	}
	data["Version"] = s.cfg.Version
	data["Page"] = page
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	if err := s.pages[page].ExecuteTemplate(w, "layout", data); err != nil {
		s.log.Error("render", "page", page, "err", err)
	}
}

func (s *Server) fail(w http.ResponseWriter, err error) {
	if errors.Is(err, store.ErrNotFound) {
		http.Error(w, err.Error(), http.StatusNotFound)
		return
	}
	s.log.Error("dashboard error", "err", err)
	http.Error(w, "internal error", http.StatusInternalServerError)
}

// since says how long ago t was, for an instance's uptime.
func since(t time.Time) string {
	if t.IsZero() {
		return "-"
	}
	return time.Since(t).Truncate(time.Second).String()
}
