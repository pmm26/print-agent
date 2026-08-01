package api

import (
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	"print-agent/internal/events"
)

// maxRequestBody bounds any POS-facing request body.
const maxRequestBody = 256 * 1024

// isLocalOrigin reports whether the Origin header belongs to the agent's own
// loopback UI (the embedded dashboard making same-host fetches).
func isLocalOrigin(origin string) bool {
	return strings.HasPrefix(origin, "http://127.0.0.1:") ||
		strings.HasPrefix(origin, "http://localhost:") ||
		strings.HasPrefix(origin, "https://127.0.0.1:") ||
		strings.HasPrefix(origin, "https://localhost:")
}

// posCORS gates the POS-facing endpoints:
//   - No Origin header: a local non-browser client (curl, tests) — allowed.
//   - Local origin: the embedded dashboard — allowed.
//   - The configured POS origin: CORS headers set; bearer token enforced by
//     requireAuth.
//   - Anything else: rejected and audited.
func (s *Server) posCORS(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		origin := r.Header.Get("Origin")
		if origin != "" && !isLocalOrigin(origin) {
			allowed, _ := s.configRepo.GetSetting("allowed_origin")
			if allowed == "" || origin != allowed {
				s.bus.Publish(events.Event{Type: events.AuthDenied,
					Message: fmt.Sprintf("origin %q rejected for %s %s", origin, r.Method, r.URL.Path)})
				http.Error(w, "origin not allowed", http.StatusForbidden)
				return
			}
			h := w.Header()
			h.Set("Access-Control-Allow-Origin", allowed)
			h.Set("Vary", "Origin")
			if r.Method == http.MethodOptions {
				h.Set("Access-Control-Allow-Methods", "GET, POST, OPTIONS")
				h.Set("Access-Control-Allow-Headers", "Authorization, Content-Type, Idempotency-Key")
				h.Set("Access-Control-Max-Age", "600")
				// Legacy Chromium Private Network Access preflights.
				if r.Header.Get("Access-Control-Request-Private-Network") == "true" {
					h.Set("Access-Control-Allow-Private-Network", "true")
				}
				w.WriteHeader(http.StatusNoContent)
				return
			}
		}
		next.ServeHTTP(w, r)
	})
}

// requireAuth enforces the bearer token on cross-origin POS requests. Local
// callers (no Origin, or the loopback dashboard) are exempt: they already
// have local access to the machine.
func (s *Server) requireAuth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		origin := r.Header.Get("Origin")
		if origin != "" && !isLocalOrigin(origin) {
			token := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
			if !s.auth.ValidateToken(token) {
				s.bus.Publish(events.Event{Type: events.AuthDenied,
					Message: fmt.Sprintf("invalid token from origin %q for %s %s", origin, r.Method, r.URL.Path)})
				http.Error(w, "invalid or missing token", http.StatusUnauthorized)
				return
			}
		}
		next.ServeHTTP(w, r)
	})
}

// localOnly rejects any cross-origin browser request. Management endpoints
// are reachable only from the embedded dashboard or local tools.
func (s *Server) localOnly(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		origin := r.Header.Get("Origin")
		if origin != "" && !isLocalOrigin(origin) {
			s.bus.Publish(events.Event{Type: events.AuthDenied,
				Message: fmt.Sprintf("origin %q rejected for admin endpoint %s", origin, r.URL.Path)})
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// rateLimiter is a small token bucket shared by the POS endpoints.
type rateLimiter struct {
	mu     sync.Mutex
	tokens float64
	max    float64
	rate   float64 // tokens per second
	last   time.Time
}

func newRateLimiter(ratePerSec, burst float64) *rateLimiter {
	return &rateLimiter{tokens: burst, max: burst, rate: ratePerSec, last: time.Now()}
}

func (l *rateLimiter) allow() bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := time.Now()
	l.tokens = min(l.max, l.tokens+now.Sub(l.last).Seconds()*l.rate)
	l.last = now
	if l.tokens < 1 {
		return false
	}
	l.tokens--
	return true
}

func (s *Server) rateLimit(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !s.limiter.allow() {
			http.Error(w, "rate limit exceeded", http.StatusTooManyRequests)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func (s *Server) limitBody(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		r.Body = http.MaxBytesReader(w, r.Body, maxRequestBody)
		next.ServeHTTP(w, r)
	})
}

func chain(h http.Handler, middleware ...func(http.Handler) http.Handler) http.Handler {
	for i := len(middleware) - 1; i >= 0; i-- {
		h = middleware[i](h)
	}
	return h
}
