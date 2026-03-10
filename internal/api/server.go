package api

import (
	"log"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	chimw "github.com/go-chi/chi/v5/middleware"
	"github.com/paasd/paasd/internal/db"
	"github.com/paasd/paasd/internal/middleware"
)

type Server struct {
	store     *db.Store
	masterKey []byte
	devMode   bool
	router    chi.Router
}

func NewServer(store *db.Store, masterKey []byte, devMode bool) *Server {
	s := &Server{
		store:     store,
		masterKey: masterKey,
		devMode:   devMode,
	}
	s.setupRoutes()
	return s
}

func (s *Server) setupRoutes() {
	r := chi.NewRouter()

	r.Use(chimw.RequestID)
	r.Use(chimw.Logger)
	r.Use(chimw.Recoverer)
	r.Use(chimw.Timeout(30 * time.Second))
	r.Use(jsonContentType)
	r.Use(maxBodySize(1 << 20))

	// Enforce HTTPS via X-Forwarded-Proto (Traefik sets this)
	if !s.devMode {
		r.Use(requireHTTPS)
	}

	// Public routes
	r.Get("/v1/system/health", s.handleHealth)
	r.Post("/v1/tenants/register", s.handleTenantRegister)

	// Authenticated routes
	r.Group(func(r chi.Router) {
		r.Use(middleware.Auth(s.store.StateDB, s.masterKey))
		rl := middleware.NewRateLimiter(100, 200)
		r.Use(rl.Middleware)
		idem := middleware.NewIdempotencyStore()
		r.Use(idem.Middleware)

		r.Get("/v1/system/health/detailed", s.handleHealthDetailed)

		r.Get("/v1/tenant", s.handleTenantGet)
		r.Patch("/v1/tenant", s.handleTenantUpdate)
		r.Delete("/v1/tenant", s.handleTenantDelete)

		r.Post("/v1/auth/keys", s.handleKeyCreate)
		r.Get("/v1/auth/keys", s.handleKeyList)
		r.Delete("/v1/auth/keys/{keyID}", s.handleKeyRevoke)
	})

	s.router = r
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.router.ServeHTTP(w, r)
}

func (s *Server) ListenAndServe(addr string) error {
	log.Printf("starting server on %s", addr)
	log.Printf("WARNING: server is listening on plain HTTP. Ensure Traefik or another TLS-terminating proxy is in front of this service.")
	return http.ListenAndServe(addr, s)
}

func jsonContentType(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		next.ServeHTTP(w, r)
	})
}

func maxBodySize(maxBytes int64) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			r.Body = http.MaxBytesReader(w, r.Body, maxBytes)
			next.ServeHTTP(w, r)
		})
	}
}

// requireHTTPS rejects requests not arriving via TLS-terminating proxy.
// Traefik sets X-Forwarded-Proto: https for all TLS-terminated connections.
func requireHTTPS(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		proto := r.Header.Get("X-Forwarded-Proto")
		if proto != "https" {
			http.Error(w, `{"error":"HTTPS required"}`, http.StatusForbidden)
			return
		}
		next.ServeHTTP(w, r)
	})
}
