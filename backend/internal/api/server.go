package api

import (
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/vihor3/searchmeld/backend/internal/config"
)

type Server struct {
	cfg     config.Config
	log     requestLogger
	router  chi.Router
	healthy func() bool
}

// NewServer wires request identity and peer handling before route middleware,
// placing the admin policy ahead of public CORS. It registers health checks
// without starting a listener; application routes must be mounted separately.
func NewServer(cfg config.Config, log requestLogger) *Server {
	server := &Server{cfg: cfg, log: log, healthy: func() bool { return true }}
	r := chi.NewRouter()
	r.Use(requestIDMiddleware)
	r.Use(trustedProxyIPMiddleware)
	r.Use(securityHeadersMiddleware)
	r.Use(adminBrowserMiddleware(cfg.AdminPublicOrigin, cfg.CorsOrigins))
	r.Use(corsMiddleware(cfg.CorsOrigins))
	r.Use(bodyLimitMiddleware(cfg.RequestBodyLimitBytes))
	r.Use(loggingMiddleware(log))
	r.Get("/healthz", server.healthz)
	server.router = r
	return server
}

func (s *Server) Router() http.Handler {
	return s.router
}

func (s *Server) Mount(fn func(chi.Router)) {
	fn(s.router)
}

func (s *Server) SetHealth(fn func() bool) {
	s.healthy = fn
}

func (s *Server) healthz(w http.ResponseWriter, r *http.Request) {
	if !s.healthy() {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"status": "unhealthy"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}
