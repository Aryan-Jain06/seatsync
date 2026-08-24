package handlers

import (
	"context"
	"net/http"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/redis/go-redis/v9"

	"github.com/Aryan-Jain06/seatsync/backend/internal/httpx"
)

// HealthHandler reports whether the service and its dependencies are usable.
type HealthHandler struct {
	pool  *pgxpool.Pool
	redis *redis.Client
}

// NewHealthHandler builds a HealthHandler. redis may be nil before Phase 3
// wires it in, in which case it is simply not reported.
func NewHealthHandler(pool *pgxpool.Pool, rdb *redis.Client) *HealthHandler {
	return &HealthHandler{pool: pool, redis: rdb}
}

type healthResponse struct {
	Status     string            `json:"status"`
	Components map[string]string `json:"components"`
}

// Live handles GET /health: is the process up at all.
func (h *HealthHandler) Live(w http.ResponseWriter, r *http.Request) {
	httpx.JSON(w, http.StatusOK, healthResponse{Status: "ok", Components: map[string]string{}})
}

// Ready handles GET /health/ready: can the process actually serve traffic.
// It returns 503 when a dependency is down so a load balancer stops sending
// requests here rather than watching them fail.
func (h *HealthHandler) Ready(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
	defer cancel()

	components := make(map[string]string, 2)
	healthy := true

	if err := h.pool.Ping(ctx); err != nil {
		components["postgres"] = "unavailable: " + err.Error()
		healthy = false
	} else {
		components["postgres"] = "ok"
	}

	if h.redis != nil {
		if err := h.redis.Ping(ctx).Err(); err != nil {
			components["redis"] = "unavailable: " + err.Error()
			healthy = false
		} else {
			components["redis"] = "ok"
		}
	}

	status := http.StatusOK
	body := healthResponse{Status: "ok", Components: components}
	if !healthy {
		status = http.StatusServiceUnavailable
		body.Status = "degraded"
	}
	httpx.JSON(w, status, body)
}
