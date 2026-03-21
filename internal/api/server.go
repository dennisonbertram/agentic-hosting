package api

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/dennisonbertram/agentic-hosting/internal/builds"
	"github.com/dennisonbertram/agentic-hosting/internal/databases"
	"github.com/dennisonbertram/agentic-hosting/internal/db"
	"github.com/dennisonbertram/agentic-hosting/internal/docker"
	"github.com/dennisonbertram/agentic-hosting/internal/httpx"
	"github.com/dennisonbertram/agentic-hosting/internal/kanbans"
	"github.com/dennisonbertram/agentic-hosting/internal/middleware"
	"github.com/dennisonbertram/agentic-hosting/internal/services"
	"github.com/dennisonbertram/agentic-hosting/internal/snapshots"
	"github.com/go-chi/chi/v5"
	chimw "github.com/go-chi/chi/v5/middleware"
)

type ServerConfig struct {
	Store            *db.Store
	MasterKey        []byte
	DevMode          bool
	BootstrapToken   string
	OpenRegistration bool
	Docker           docker.Client
	BuildManager     *builds.Manager
	DatabaseManager  databaseManager
	KanbanManager    kanbanManager
	BaseDomain       string
	TraefikConfigDir string
}

type kanbanManager interface {
	Create(ctx context.Context, tenantID string) (*kanbans.Kanban, error)
	Get(ctx context.Context, tenantID string) (*kanbans.Kanban, error)
	GetAdminToken(ctx context.Context, tenantID string) (string, error)
	Delete(ctx context.Context, tenantID string) error
	StopForTenant(ctx context.Context, tenantID string)
}

type databaseManager interface {
	Create(ctx context.Context, tenantID string, req databases.CreateRequest) (*databases.Database, error)
	List(ctx context.Context, tenantID string) ([]*databases.Database, error)
	ListPaginated(ctx context.Context, tenantID string, limit, offset int) ([]*databases.Database, error)
	Get(ctx context.Context, tenantID, dbID string) (*databases.Database, error)
	GetConnectionString(ctx context.Context, tenantID, dbID string) (string, error)
	Delete(ctx context.Context, tenantID, dbID string) error
	StopAllForTenant(ctx context.Context, tenantID string)
}

type Server struct {
	store             *db.Store
	masterKey         []byte
	devMode           bool
	bootstrapToken    string
	openRegistration  bool
	baseDomain        string
	router            chi.Router
	authMW            func(http.Handler) http.Handler
	authInvalidator   *middleware.AuthCacheInvalidator
	svcManager        *services.Manager
	snapshotManager   *snapshots.Manager
	buildManager      *builds.Manager
	dbManager         databaseManager
	kanbanManager     kanbanManager
	authRateLimiter   *middleware.RateLimiter
	globalRateLimiter *middleware.GlobalRateLimiter
	idempotencyStore  *middleware.IdempotencyStore
}

func NewServer(cfg ServerConfig) *Server {
	if cfg.Store == nil || cfg.Store.StateDB == nil {
		panic("ah: NewServer requires a non-nil Store with StateDB")
	}
	// Initialize auth middleware and cache invalidator early so they are
	// guaranteed non-nil before any request can arrive.
	authMW, authInvalidator := middleware.Auth(cfg.Store.StateDB, cfg.MasterKey)
	rl := middleware.NewRateLimiter(100, 200)
	globalRL := middleware.NewGlobalRateLimiter(500, 1000)
	idem := middleware.NewIdempotencyStore()

	var svcMgr *services.Manager
	var snapMgr *snapshots.Manager
	if cfg.Docker != nil {
		svcMgr = services.NewManager(cfg.Store.StateDB, cfg.Docker, cfg.MasterKey, cfg.BaseDomain, cfg.TraefikConfigDir)
		snapMgr = snapshots.NewManager(cfg.Store.StateDB, cfg.Docker, cfg.MasterKey)
	}

	s := &Server{
		store:             cfg.Store,
		masterKey:         cfg.MasterKey,
		devMode:           cfg.DevMode,
		bootstrapToken:    cfg.BootstrapToken,
		openRegistration:  cfg.OpenRegistration,
		baseDomain:        cfg.BaseDomain,
		authInvalidator:   authInvalidator,
		authMW:            authMW,
		svcManager:        svcMgr,
		snapshotManager:   snapMgr,
		buildManager:      cfg.BuildManager,
		dbManager:         cfg.DatabaseManager,
		kanbanManager:     cfg.KanbanManager,
		authRateLimiter:   rl,
		globalRateLimiter: globalRL,
		idempotencyStore:  idem,
	}
	s.setupRoutes()
	return s
}

func (s *Server) setupRoutes() {
	r := chi.NewRouter()

	r.Use(chimw.RequestID)
	// chi.Logger logs method/path/status/latency only — never headers or body.
	// Authorization and bootstrap tokens are never logged.
	r.Use(chimw.Logger)
	r.Use(chimw.Recoverer)
	r.Use(maxBodySize(1 << 20))
	// Global concurrency limiter: cap in-flight requests to prevent goroutine exhaustion
	r.Use(chimw.Throttle(200))

	// Strip forwarded headers from non-loopback requests to prevent spoofing.
	// This runs unconditionally (including dev mode) as a defense-in-depth measure.
	r.Use(stripUntrustedForwardedHeaders)

	// Enforce HTTPS: only trust X-Forwarded-Proto from loopback (trusted proxy).
	// Server binds to 127.0.0.1 by default; even if --listen-addr overrides this,
	// the HTTPS check only trusts the header from loopback RemoteAddr.
	if !s.devMode {
		r.Use(requireHTTPS)
	}

	// Public routes keep the standard request timeout.
	r.Group(func(r chi.Router) {
		r.Use(chimw.Timeout(30 * time.Second))
		r.Get("/v1/system/health", s.handleHealth)
		r.Post("/v1/tenants/register", s.handleTenantRegister)
		// Recovery endpoint: authenticated by bootstrap token, not an API key.
		// Allows tenants who have lost all their keys to obtain a new one.
		r.Post("/v1/auth/recover", s.handleKeyRecover)
	})

	// Authenticated short-lived routes keep the standard request timeout.
	r.Group(func(r chi.Router) {
		r.Use(s.authMW)
		r.Use(s.authRateLimiter.Middleware)
		r.Use(s.globalRateLimiter.Middleware)
		r.Use(s.idempotencyStore.Middleware)
		r.Use(chimw.Timeout(30 * time.Second))

		r.Get("/v1/system/health/detailed", s.handleHealthDetailed)

		r.Get("/v1/tenant", s.handleTenantGet)
		r.Get("/v1/tenant/usage", s.handleTenantUsage)
		r.Patch("/v1/tenant", s.handleTenantUpdate)
		r.Delete("/v1/tenant", s.handleTenantDelete)

		r.Post("/v1/auth/keys", s.handleKeyCreate)
		r.Get("/v1/auth/keys", s.handleKeyList)
		r.Delete("/v1/auth/keys/{keyID}", s.handleKeyRevoke)

		r.Get("/v1/activity", s.handleActivityList)

		// Service routes
		r.Post("/v1/services", s.handleServiceCreate)
		r.Get("/v1/services", s.handleServiceList)
		r.Get("/v1/services/{serviceID}", s.handleServiceGet)
		r.Patch("/v1/services/{serviceID}", s.handleServiceUpdate)
		r.Delete("/v1/services/{serviceID}", s.handleServiceDelete)
		r.Post("/v1/services/{serviceID}/start", s.handleServiceStart)
		r.Post("/v1/services/{serviceID}/stop", s.handleServiceStop)
		r.Post("/v1/services/{serviceID}/restart", s.handleServiceRestart)
		r.Post("/v1/services/{serviceID}/redeploy", s.handleServiceRedeploy)
		r.Post("/v1/services/{serviceID}/reset", s.handleServiceReset)
		r.Get("/v1/services/{serviceID}/deployments", s.handleServiceDeployments)
		r.Get("/v1/services/{serviceID}/env", s.handleServiceEnvGet)
		r.Post("/v1/services/{serviceID}/env", s.handleServiceEnvSet)
		r.Delete("/v1/services/{serviceID}/env/{key}", s.handleServiceEnvDelete)

		// Snapshot routes
		r.Post("/v1/services/{serviceID}/snapshots", s.handleSnapshotCreate)
		r.Get("/v1/snapshots", s.handleSnapshotList)
		r.Get("/v1/snapshots/{snapshotID}", s.handleSnapshotGet)
		r.Delete("/v1/snapshots/{snapshotID}", s.handleSnapshotDelete)

		// Build routes
		r.Get("/v1/builds", s.handleBuildListAll)
		r.Post("/v1/services/{serviceID}/builds", s.handleBuildCreate)
		r.Get("/v1/services/{serviceID}/builds", s.handleBuildList)
		r.Get("/v1/services/{serviceID}/builds/{buildID}", s.handleBuildGet)
		// Build log streaming is in a separate group (no 30s timeout)
		r.Delete("/v1/services/{serviceID}/builds/{buildID}", s.handleBuildCancel)

		// Database routes (except POST which needs longer timeout)
		r.Get("/v1/databases", s.handleDatabaseList)
		r.Get("/v1/databases/{dbID}", s.handleDatabaseGet)
		r.Get("/v1/databases/{dbID}/connection-string", s.handleDatabaseConnectionString)
		r.Delete("/v1/databases/{dbID}", s.handleDatabaseDelete)

		// Kanban routes (except POST which needs longer timeout)
		r.Get("/v1/kanban", s.handleKanbanGet)
		r.Get("/v1/kanban/admin-token", s.handleKanbanAdminToken)
		r.Delete("/v1/kanban", s.handleKanbanDelete)
	})

	// Long-running endpoints share auth/rate-limit middleware but intentionally
	// skip the standard timeout so streaming and provisioning can complete.
	r.Group(func(r chi.Router) {
		r.Use(s.authMW)
		r.Use(s.authRateLimiter.Middleware)
		r.Use(s.globalRateLimiter.Middleware)
		r.Use(s.idempotencyStore.Middleware)
		r.Get("/v1/services/{serviceID}/logs", s.handleServiceLogs)
		r.Get("/v1/services/{serviceID}/builds/{buildID}/logs", s.handleBuildLogs)
		r.Post("/v1/databases", s.handleDatabaseCreate)
		r.Post("/v1/kanban", s.handleKanbanCreate)
	})

	s.router = r
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.router.ServeHTTP(w, r)
}

// writeError delegates to httpx.WriteError for consistent JSON error responses.
// All handlers in the api package should use this.
func writeError(w http.ResponseWriter, code int, message string) {
	httpx.WriteError(w, code, message)
}

// writeJSON delegates to httpx.WriteJSON for consistent JSON success responses.
// All handlers in the api package should use this.
func writeJSON(w http.ResponseWriter, code int, v any) {
	httpx.WriteJSON(w, code, v)
}

// writeDecodeError writes a 413 if the error is from MaxBytesReader, otherwise 400.
func writeDecodeError(w http.ResponseWriter, err error) {
	var maxBytesErr *http.MaxBytesError
	if errors.As(err, &maxBytesErr) {
		writeError(w, http.StatusRequestEntityTooLarge, "request body too large")
		return
	}
	writeError(w, http.StatusBadRequest, "invalid request body")
}

func maxBodySize(maxBytes int64) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			r.Body = http.MaxBytesReader(w, r.Body, maxBytes)
			next.ServeHTTP(w, r)
		})
	}
}

// isLoopback checks if the remote address is from a loopback interface.
func isLoopback(remoteAddr string) bool {
	host, _, err := net.SplitHostPort(remoteAddr)
	if err != nil {
		host = remoteAddr
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// stripUntrustedForwardedHeaders removes proxy headers from non-loopback
// requests to prevent spoofing. Runs unconditionally (including dev mode).
func stripUntrustedForwardedHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !isLoopback(r.RemoteAddr) {
			r.Header.Del("X-Forwarded-For")
			r.Header.Del("X-Forwarded-Proto")
			r.Header.Del("X-Real-Ip")
		}
		next.ServeHTTP(w, r)
	})
}

// requireHTTPS rejects requests not arriving via TLS-terminating proxy.
// Only trusts X-Forwarded-Proto when RemoteAddr is loopback (i.e., from the
// local Traefik proxy). Direct external connections cannot spoof this header.
// Header stripping is handled by stripUntrustedForwardedHeaders above.
func requireHTTPS(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if isLoopback(r.RemoteAddr) {
			proto := r.Header.Get("X-Forwarded-Proto")
			if proto != "https" {
				writeError(w, http.StatusForbidden, "HTTPS required")
				return
			}
		} else {
			if r.TLS == nil {
				writeError(w, http.StatusForbidden, "HTTPS required")
				return
			}
		}
		next.ServeHTTP(w, r)
	})
}

// normalizeIP extracts a validated IP string from a value that may be
// "ip", "ip:port", or "[ip]:port". Returns "" if no valid IP is found.
func normalizeIP(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return ""
	}
	// Try host:port split first
	if host, _, err := net.SplitHostPort(s); err == nil {
		s = host
	}
	if ip := net.ParseIP(s); ip != nil {
		return ip.String()
	}
	return ""
}

// parsePagination extracts limit and offset query parameters with defaults.
func parsePagination(r *http.Request) (limit, offset int, err error) {
	limit = 100
	if raw := r.URL.Query().Get("limit"); raw != "" {
		limit, err = strconv.Atoi(raw)
		if err != nil || limit < 1 || limit > 200 {
			return 0, 0, fmt.Errorf("limit must be between 1 and 200")
		}
	}
	if raw := r.URL.Query().Get("offset"); raw != "" {
		offset, err = strconv.Atoi(raw)
		if err != nil || offset < 0 {
			return 0, 0, fmt.Errorf("offset must be a non-negative integer")
		}
	}
	return limit, offset, nil
}

// trustedRealIP extracts client IP from X-Real-Ip ONLY when the request comes
// from a trusted loopback proxy. X-Forwarded-For is NOT used because clients can
// spoof it and some proxy configs preserve the client-provided chain.
// Traefik always overwrites X-Real-Ip with the direct connection IP, making it
// the only trustworthy source. Falls back to RemoteAddr.
func trustedRealIP(r *http.Request) string {
	if isLoopback(r.RemoteAddr) {
		// Behind trusted proxy: prefer X-Real-Ip.
		if ip := normalizeIP(r.Header.Get("X-Real-Ip")); ip != "" {
			return ip
		}
		// If X-Real-Ip is not set (e.g., direct connection without proxy),
		// use RemoteAddr. This is safe because stripUntrustedForwardedHeaders
		// already stripped any spoofed headers from non-loopback sources.
		// Note: callers should check for empty return and reject if needed.
		return normalizeIP(r.RemoteAddr)
	}
	return normalizeIP(r.RemoteAddr)
}
