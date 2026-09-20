package http

import (
	"fmt"
	"io/fs"
	"log/slog"
	"net/http"
	"time"

	"github.com/relaxart/dev-pulse/internal/collector"
	"github.com/relaxart/dev-pulse/internal/config"
	"github.com/relaxart/dev-pulse/internal/database"
	"github.com/relaxart/dev-pulse/internal/metrics"
)

// Server renders the dashboard from PostgreSQL.
type Server struct {
	cfg       *config.Config
	db        *database.DB
	worker    *collector.Worker
	log       *slog.Logger
	renderer  *renderer
	version   string
	startedAt time.Time
	assets    fs.FS
}

// New builds the HTTP server.
func New(cfg *config.Config, db *database.DB, worker *collector.Worker, assets fs.FS, version string, log *slog.Logger) (*Server, error) {
	r, err := newRenderer(assets)
	if err != nil {
		return nil, err
	}
	static, err := fs.Sub(assets, "static")
	if err != nil {
		return nil, fmt.Errorf("locate static assets: %w", err)
	}
	return &Server{
		cfg:       cfg,
		db:        db,
		worker:    worker,
		log:       log,
		renderer:  r,
		version:   version,
		startedAt: time.Now(),
		assets:    static,
	}, nil
}

// Handler builds the router.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()

	mux.HandleFunc("GET /{$}", s.handleDashboard)
	mux.HandleFunc("GET /contributors", s.handleContributors)
	mux.HandleFunc("GET /contributors/{login}", s.handleContributor)
	mux.HandleFunc("GET /repositories", s.handleRepositories)
	mux.HandleFunc("GET /repositories/{name}", s.handleRepository)
	mux.HandleFunc("GET /status", s.handleStatus)
	mux.HandleFunc("POST /status/sync", s.handleSyncNow)
	mux.HandleFunc("GET /health", s.handleHealth)
	mux.HandleFunc("GET /ready", s.handleReady)

	fileServer := http.FileServer(http.FS(s.assets))
	mux.Handle("GET /static/", cacheStatic(http.StripPrefix("/static/", fileServer)))

	mux.HandleFunc("GET /", s.handleNotFound)

	return s.recoverPanics(s.logRequests(securityHeaders(mux)))
}

func (s *Server) handleNotFound(w http.ResponseWriter, r *http.Request) {
	pc, ok := s.context(w, r, "")
	if !ok {
		return
	}
	s.notFound(w, r, pc.Layout, "The page "+r.URL.Path+" does not exist.")
}

type errorView struct {
	layoutData
	Code    int
	Heading string
	Message string
}

func (s *Server) notFound(w http.ResponseWriter, r *http.Request, l layoutData, message string) {
	l.Title = "Not found"
	view := errorView{layoutData: l, Code: http.StatusNotFound, Heading: "Not found", Message: message}
	s.render(w, r, http.StatusNotFound, "error.html", view)
}

// serverError logs the full error and shows a short, secret-free message.
func (s *Server) serverError(w http.ResponseWriter, r *http.Request, err error) {
	s.log.Error("request failed", "path", r.URL.Path, "error", err)
	view := errorView{
		layoutData: layoutData{
			AppName: AppName,
			Title:   "Error",
			Path:    "/",
			Org:     s.cfg.GitHubOrg,
			Range:   defaultRange(),
			Now:     time.Now().UTC(),
		},
		Code:    http.StatusInternalServerError,
		Heading: "Something went wrong",
		Message: "The dashboard could not read from PostgreSQL. The application log has the details.",
	}
	s.render(w, r, http.StatusInternalServerError, "error.html", view)
}

func (s *Server) render(w http.ResponseWriter, r *http.Request, status int, page string, data any) {
	if err := s.renderer.render(w, status, page, data); err != nil {
		s.log.Error("template rendering failed", "page", page, "path", r.URL.Path, "error", err)
	}
}

// recoverPanics keeps a handler panic from taking down the process.
func (s *Server) recoverPanics(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if rec := recover(); rec != nil {
				s.log.Error("handler panicked", "path", r.URL.Path, "panic", fmt.Sprint(rec))
				http.Error(w, "internal server error", http.StatusInternalServerError)
			}
		}()
		next.ServeHTTP(w, r)
	})
}

type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (s *statusRecorder) WriteHeader(code int) {
	s.status = code
	s.ResponseWriter.WriteHeader(code)
}

func (s *Server) logRequests(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(rec, r)
		s.log.Debug("http request",
			"method", r.Method,
			"path", r.URL.Path,
			"query", r.URL.RawQuery,
			"status", rec.status,
			"duration_ms", time.Since(start).Milliseconds(),
		)
	})
}

// securityHeaders sets a conservative CSP. Bootstrap, Material Symbols and
// Chart.js are loaded from jsDelivr and Google Fonts; avatars come from GitHub.
func securityHeaders(next http.Handler) http.Handler {
	const csp = "default-src 'self'; " +
		"script-src 'self' https://cdn.jsdelivr.net 'unsafe-inline'; " +
		"style-src 'self' https://cdn.jsdelivr.net https://fonts.googleapis.com 'unsafe-inline'; " +
		"font-src 'self' https://fonts.gstatic.com https://cdn.jsdelivr.net; " +
		"img-src 'self' data: https://avatars.githubusercontent.com https://*.githubusercontent.com; " +
		"connect-src 'self'; frame-ancestors 'none'; base-uri 'self'; form-action 'self'"

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Security-Policy", csp)
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Referrer-Policy", "no-referrer")
		w.Header().Set("X-Frame-Options", "DENY")
		next.ServeHTTP(w, r)
	})
}

func cacheStatic(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "public, max-age=3600")
		next.ServeHTTP(w, r)
	})
}

// defaultRange is used by the error page, which has no validated filter.
func defaultRange() metrics.Range {
	r, _ := metrics.ResolveRange(metrics.DefaultPeriod, "", "", time.Now())
	return r
}
