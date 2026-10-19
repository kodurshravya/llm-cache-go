// Package proxy is the HTTP layer: routing, cache policy, coalescing,
// streaming tee and replay, and admin routes.
package proxy

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"time"

	"golang.org/x/sync/singleflight"

	"github.com/kodurshravya/llm-cache-go/internal/cache"
	"github.com/kodurshravya/llm-cache-go/internal/keys"
	"github.com/kodurshravya/llm-cache-go/internal/metrics"
)

// maxRequestBytes caps incoming request bodies.
const maxRequestBytes = 10 << 20

// Config holds the proxy settings.
type Config struct {
	UpstreamURL     *url.URL
	UpstreamTimeout time.Duration
	MaxEntryBytes   int
	AdminToken      string
}

// Server is the caching proxy.
type Server struct {
	cfg     Config
	store   *cache.Store
	metrics *metrics.Metrics
	log     *slog.Logger
	client  *http.Client
	group   singleflight.Group
}

// New builds a Server.
func New(cfg Config, store *cache.Store, m *metrics.Metrics, log *slog.Logger) *Server {
	if log == nil {
		log = slog.Default()
	}
	t := http.DefaultTransport.(*http.Transport).Clone()
	// The default of 2 idle connections per host throttles load tests.
	t.MaxIdleConns = 512
	t.MaxIdleConnsPerHost = 256
	t.IdleConnTimeout = 90 * time.Second
	return &Server{
		cfg:     cfg,
		store:   store,
		metrics: m,
		log:     log,
		client:  &http.Client{Transport: t}, // per-request timeouts come from contexts
	}
}

// Handler returns the routes.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/chat/completions", s.cached(keys.KindChat, "/v1/chat/completions"))
	mux.HandleFunc("POST /v1/embeddings", s.cached(keys.KindEmbeddings, "/v1/embeddings"))
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
	})
	mux.HandleFunc("GET /readyz", s.readyz)
	mux.Handle("GET /metrics", s.metrics.Handler())
	mux.HandleFunc("POST /admin/templates/{name}/bump", s.admin(s.bumpTemplate))
	mux.HandleFunc("DELETE /admin/cache", s.admin(s.flushModel))
	return mux
}

func (s *Server) readyz(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
	defer cancel()
	if err := s.store.Ping(ctx); err != nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"status": "unready", "redis": err.Error()})
		return
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, s.cfg.UpstreamURL.String(), nil)
	if err != nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"status": "unready", "upstream": err.Error()})
		return
	}
	resp, err := s.client.Do(req)
	if err != nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"status": "unready", "upstream": err.Error()})
		return
	}
	resp.Body.Close() // any HTTP response means upstream is reachable
	writeJSON(w, http.StatusOK, map[string]string{"status": "ready"})
}

func (s *Server) admin(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if s.cfg.AdminToken == "" {
			writeError(w, http.StatusServiceUnavailable, "admin routes are disabled: set ADMIN_TOKEN")
			return
		}
		got := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		if subtle.ConstantTimeCompare([]byte(got), []byte(s.cfg.AdminToken)) != 1 {
			writeError(w, http.StatusUnauthorized, "invalid admin token")
			return
		}
		next(w, r)
	}
}

func (s *Server) bumpTemplate(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	v, err := s.store.BumpTemplate(r.Context(), name)
	if err != nil {
		writeError(w, http.StatusServiceUnavailable, "redis: "+err.Error())
		return
	}
	s.log.Info("template bumped", "template", name, "version", v)
	writeJSON(w, http.StatusOK, map[string]any{"template": name, "version": v})
}

func (s *Server) flushModel(w http.ResponseWriter, r *http.Request) {
	model := r.URL.Query().Get("model")
	if model == "" {
		writeError(w, http.StatusBadRequest, "missing ?model=")
		return
	}
	g, err := s.store.BumpModel(r.Context(), model)
	if err != nil {
		writeError(w, http.StatusServiceUnavailable, "redis: "+err.Error())
		return
	}
	s.log.Info("model namespace flushed", "model", model, "generation", g)
	writeJSON(w, http.StatusOK, map[string]any{"model": model, "generation": g})
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// writeError uses OpenAI's error shape so clients parse it normally.
func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]any{
		"error": map[string]string{"message": msg, "type": "llm_cache_error"},
	})
}
