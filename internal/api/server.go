// Package api exposes RateGuard over HTTP.
package api

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"log/slog"
	"math"
	"net/http"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/yourname/rateguard/internal/limiter"
	"github.com/yourname/rateguard/internal/metrics"
	"github.com/yourname/rateguard/internal/policy"
)

// Allower is the subset of *limiter.Limiter the API needs.
type Allower interface {
	Allow(ctx context.Context, clientID string, p limiter.Policy, cost int64) (limiter.Result, error)
}

// PolicyStore is the subset of *policy.Store the API needs.
type PolicyStore interface {
	Get(ctx context.Context, clientID string) limiter.Policy
	Upsert(ctx context.Context, r policy.Record) (policy.Record, error)
	GetRecord(ctx context.Context, clientID string) (policy.Record, error)
	Delete(ctx context.Context, clientID string) error
	List(ctx context.Context, limit, offset int) ([]policy.Record, error)
	Ping(ctx context.Context) error
}

// Options configures a Server.
type Options struct {
	InstanceID   string
	AdminToken   string
	FailOpen     bool
	RedisTimeout time.Duration
	RedisPing    func(ctx context.Context) error

	Limiter  Allower
	Policies PolicyStore
	Metrics  *metrics.Metrics
	Log      *slog.Logger
}

// Server wires the HTTP handlers together.
type Server struct {
	Options
	lastRedisLog atomic.Int64 // unix nanos; throttles error logging
}

func New(o Options) *Server {
	if o.RedisTimeout <= 0 {
		o.RedisTimeout = 50 * time.Millisecond
	}
	return &Server{Options: o}
}

// Handler returns the routed handler.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()

	// Data plane.
	mux.HandleFunc("POST /v1/check", s.instrument("check", s.handleCheck))
	mux.Handle("GET /v1/resource", s.instrument("resource", s.guard(s.handleResource)))

	// Control plane.
	mux.HandleFunc("GET /v1/policies", s.instrument("policy_list", s.admin(s.handleListPolicies)))
	mux.HandleFunc("GET /v1/policies/{client_id}", s.instrument("policy_get", s.admin(s.handleGetPolicy)))
	mux.HandleFunc("PUT /v1/policies/{client_id}", s.instrument("policy_put", s.admin(s.handlePutPolicy)))
	mux.HandleFunc("DELETE /v1/policies/{client_id}", s.instrument("policy_delete", s.admin(s.handleDeletePolicy)))

	// Ops.
	mux.HandleFunc("GET /healthz", s.handleHealth)
	mux.Handle("GET /metrics", s.Metrics.Handler())

	return s.withInstanceHeader(mux)
}

/* ------------------------------ decisions ------------------------------ */

type decision struct {
	Allowed      bool
	Limit        int64
	Remaining    int64
	RetryAfter   time.Duration
	NeverRefills bool
	Degraded     bool // decided without Redis (fail-open)
}

var (
	errBackendDown = errors.New("rate limiter backend unavailable")
	errBadCost     = limiter.ErrCostExceedsCapacity
)

// decide is the single code path for every rate-limit decision, so metrics and
// failure behaviour are identical for /v1/check and the guard middleware.
func (s *Server) decide(ctx context.Context, clientID string, cost int64) (decision, error) {
	start := time.Now()
	if cost < 1 {
		cost = 1
	}

	p := s.Policies.Get(ctx, clientID)
	if cost > p.Capacity {
		return decision{}, errBadCost
	}
	s.Metrics.TokensRequested.Add(float64(cost))

	rctx, cancel := context.WithTimeout(ctx, s.RedisTimeout)
	defer cancel()
	res, err := s.Limiter.Allow(rctx, clientID, p, cost)

	if err != nil {
		s.Metrics.RedisErrors.Inc()
		s.logRedisError(err)
		if s.FailOpen {
			s.record(metrics.ResultErrorOpen, start)
			return decision{Allowed: true, Limit: p.Capacity, Degraded: true}, nil
		}
		s.record(metrics.ResultErrorClosed, start)
		return decision{}, errBackendDown
	}

	result := metrics.ResultAllowed
	if !res.Allowed {
		result = metrics.ResultDenied
	}
	s.record(result, start)

	return decision{
		Allowed:      res.Allowed,
		Limit:        res.Limit,
		Remaining:    res.Remaining,
		RetryAfter:   res.RetryAfter,
		NeverRefills: res.NeverRefills,
	}, nil
}

func (s *Server) record(result string, start time.Time) {
	s.Metrics.Decisions.WithLabelValues(result).Inc()
	s.Metrics.DecisionLatency.WithLabelValues(result).Observe(time.Since(start).Seconds())
}

// logRedisError logs at most once per second so an outage can't flood logs.
func (s *Server) logRedisError(err error) {
	now := time.Now().UnixNano()
	last := s.lastRedisLog.Load()
	if now-last > int64(time.Second) && s.lastRedisLog.CompareAndSwap(last, now) {
		s.Log.Error("redis limiter error", "err", err, "fail_open", s.FailOpen)
	}
}

func setRateHeaders(w http.ResponseWriter, d decision) {
	h := w.Header()
	h.Set("X-RateLimit-Limit", strconv.FormatInt(d.Limit, 10))
	h.Set("X-RateLimit-Remaining", strconv.FormatInt(d.Remaining, 10))
	if !d.Allowed && !d.NeverRefills {
		secs := int(math.Ceil(d.RetryAfter.Seconds()))
		if secs < 1 {
			secs = 1
		}
		h.Set("Retry-After", strconv.Itoa(secs))
	}
}

// writeDecisionError maps decide() errors to HTTP responses.
func (s *Server) writeDecisionError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, errBadCost):
		writeErr(w, http.StatusBadRequest, "cost exceeds the client's bucket capacity")
	default:
		w.Header().Set("Retry-After", "1")
		writeErr(w, http.StatusServiceUnavailable, "rate limiter unavailable")
	}
}

/* ------------------------------ data plane ----------------------------- */

type checkRequest struct {
	ClientID string `json:"client_id"`
	Cost     int64  `json:"cost"`
}

type checkResponse struct {
	ClientID     string `json:"client_id"`
	Allowed      bool   `json:"allowed"`
	Limit        int64  `json:"limit"`
	Remaining    int64  `json:"remaining"`
	RetryAfterMs int64  `json:"retry_after_ms"`
	Degraded     bool   `json:"degraded,omitempty"`
}

// handleCheck: POST /v1/check {"client_id": "...", "cost": 1}
// 200 = allowed, 429 = denied.
func (s *Server) handleCheck(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, 4<<10)
	var req checkRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	if !validClientID(req.ClientID) {
		writeErr(w, http.StatusBadRequest, "client_id must be 1-128 chars of [A-Za-z0-9._:@-]")
		return
	}
	if req.Cost < 0 {
		writeErr(w, http.StatusBadRequest, "cost must be >= 1")
		return
	}

	d, err := s.decide(r.Context(), req.ClientID, req.Cost)
	if err != nil {
		s.writeDecisionError(w, err)
		return
	}

	setRateHeaders(w, d)
	status := http.StatusOK
	if !d.Allowed {
		status = http.StatusTooManyRequests
	}
	writeJSON(w, status, checkResponse{
		ClientID:     req.ClientID,
		Allowed:      d.Allowed,
		Limit:        d.Limit,
		Remaining:    d.Remaining,
		RetryAfterMs: d.RetryAfter.Milliseconds(),
		Degraded:     d.Degraded,
	})
}

// guard is middleware that rate-limits any handler by the X-Client-ID header.
// It shows how RateGuard would sit in front of a real API.
func (s *Server) guard(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		clientID := r.Header.Get("X-Client-ID")
		if !validClientID(clientID) {
			writeErr(w, http.StatusBadRequest, "missing or invalid X-Client-ID header")
			return
		}
		d, err := s.decide(r.Context(), clientID, 1)
		if err != nil {
			s.writeDecisionError(w, err)
			return
		}
		setRateHeaders(w, d)
		if !d.Allowed {
			writeErr(w, http.StatusTooManyRequests, "rate limit exceeded")
			return
		}
		next(w, r)
	}
}

func (s *Server) handleResource(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"message": "ok"})
}

/* ----------------------------- control plane --------------------------- */

func (s *Server) admin(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if s.AdminToken != "" {
			got := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
			if subtle.ConstantTimeCompare([]byte(got), []byte(s.AdminToken)) != 1 {
				writeErr(w, http.StatusUnauthorized, "invalid or missing admin token")
				return
			}
		}
		next(w, r)
	}
}

type putPolicyRequest struct {
	Capacity     int64   `json:"capacity"`
	RefillPerSec float64 `json:"refill_per_sec"`
}

func (s *Server) handlePutPolicy(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("client_id")
	if !validClientID(id) {
		writeErr(w, http.StatusBadRequest, "invalid client_id")
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 4<<10)
	var req putPolicyRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	want := policy.Record{ClientID: id, Capacity: req.Capacity, RefillPerSec: req.RefillPerSec}
	if err := want.Validate(); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	rec, err := s.Policies.Upsert(r.Context(), want)
	if err != nil {
		s.Log.Error("upsert policy", "err", err)
		writeErr(w, http.StatusInternalServerError, "failed to save policy")
		return
	}
	writeJSON(w, http.StatusOK, rec)
}

func (s *Server) handleGetPolicy(w http.ResponseWriter, r *http.Request) {
	rec, err := s.Policies.GetRecord(r.Context(), r.PathValue("client_id"))
	if errors.Is(err, policy.ErrNotFound) {
		writeErr(w, http.StatusNotFound, "no explicit policy; client uses the default")
		return
	}
	if err != nil {
		s.Log.Error("get policy", "err", err)
		writeErr(w, http.StatusInternalServerError, "failed to load policy")
		return
	}
	writeJSON(w, http.StatusOK, rec)
}

func (s *Server) handleDeletePolicy(w http.ResponseWriter, r *http.Request) {
	err := s.Policies.Delete(r.Context(), r.PathValue("client_id"))
	if errors.Is(err, policy.ErrNotFound) {
		writeErr(w, http.StatusNotFound, "policy not found")
		return
	}
	if err != nil {
		s.Log.Error("delete policy", "err", err)
		writeErr(w, http.StatusInternalServerError, "failed to delete policy")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleListPolicies(w http.ResponseWriter, r *http.Request) {
	limit := clampInt(r.URL.Query().Get("limit"), 50, 1, 500)
	offset := clampInt(r.URL.Query().Get("offset"), 0, 0, math.MaxInt32)
	recs, err := s.Policies.List(r.Context(), limit, offset)
	if err != nil {
		s.Log.Error("list policies", "err", err)
		writeErr(w, http.StatusInternalServerError, "failed to list policies")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"policies": recs, "limit": limit, "offset": offset})
}

/* --------------------------------- ops --------------------------------- */

func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 500*time.Millisecond)
	defer cancel()

	status := map[string]string{"redis": "ok", "postgres": "ok"}
	code := http.StatusOK
	if s.RedisPing != nil {
		if err := s.RedisPing(ctx); err != nil {
			status["redis"] = "unavailable"
			code = http.StatusServiceUnavailable
		}
	}
	if err := s.Policies.Ping(ctx); err != nil {
		status["postgres"] = "unavailable"
		// Postgres being down degrades policy freshness but not enforcement
		// (see policy.Store.Get), so it does not fail readiness.
	}
	writeJSON(w, code, status)
}

/* ------------------------------ middleware ----------------------------- */

func (s *Server) withInstanceHeader(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-RateGuard-Instance", s.InstanceID)
		next.ServeHTTP(w, r)
	})
}

type statusRecorder struct {
	http.ResponseWriter
	code int
}

func (sr *statusRecorder) WriteHeader(c int) {
	sr.code = c
	sr.ResponseWriter.WriteHeader(c)
}

func (s *Server) instrument(route string, next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		sr := &statusRecorder{ResponseWriter: w, code: http.StatusOK}
		next(sr, r)
		s.Metrics.HTTPRequests.WithLabelValues(route, strconv.Itoa(sr.code)).Inc()
		s.Metrics.HTTPLatency.WithLabelValues(route).Observe(time.Since(start).Seconds())
	}
}

/* ------------------------------- helpers ------------------------------- */

func validClientID(id string) bool {
	if id == "" || len(id) > 128 {
		return false
	}
	for _, c := range id {
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9':
		case c == '.', c == '_', c == ':', c == '@', c == '-':
		default:
			return false
		}
	}
	return true
}

func clampInt(s string, def, lo, hi int) int {
	n, err := strconv.Atoi(s)
	if err != nil {
		return def
	}
	if n < lo {
		return lo
	}
	if n > hi {
		return hi
	}
	return n
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}
