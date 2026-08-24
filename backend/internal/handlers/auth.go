// Package handlers contains the HTTP layer: request decoding, delegation to
// services, and response rendering. Handlers hold no business logic.
package handlers

import (
	"net/http"

	"github.com/Aryan-Jain06/seatsync/backend/internal/httpx"
	"github.com/Aryan-Jain06/seatsync/backend/internal/middleware"
	"github.com/Aryan-Jain06/seatsync/backend/internal/services"
)

// AuthHandler serves the authentication endpoints.
type AuthHandler struct {
	auth *services.AuthService
}

// NewAuthHandler builds an AuthHandler.
func NewAuthHandler(auth *services.AuthService) *AuthHandler {
	return &AuthHandler{auth: auth}
}

type registerRequest struct {
	Email    string `json:"email"`
	Password string `json:"password"`
	Name     string `json:"name"`
}

type loginRequest struct {
	Email    string `json:"email"`
	Password string `json:"password"`
}

type refreshRequest struct {
	RefreshToken string `json:"refresh_token"`
}

// Register handles POST /auth/register.
func (h *AuthHandler) Register(w http.ResponseWriter, r *http.Request) {
	var req registerRequest
	if err := httpx.DecodeJSON(w, r, &req); err != nil {
		httpx.Error(w, r, err)
		return
	}

	result, err := h.auth.Register(r.Context(), req.Email, req.Password, req.Name)
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	httpx.JSON(w, http.StatusCreated, result)
}

// Login handles POST /auth/login.
func (h *AuthHandler) Login(w http.ResponseWriter, r *http.Request) {
	var req loginRequest
	if err := httpx.DecodeJSON(w, r, &req); err != nil {
		httpx.Error(w, r, err)
		return
	}

	result, err := h.auth.Login(r.Context(), req.Email, req.Password)
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	httpx.JSON(w, http.StatusOK, result)
}

// Refresh handles POST /auth/refresh, rotating the presented refresh token.
func (h *AuthHandler) Refresh(w http.ResponseWriter, r *http.Request) {
	var req refreshRequest
	if err := httpx.DecodeJSON(w, r, &req); err != nil {
		httpx.Error(w, r, err)
		return
	}

	result, err := h.auth.Refresh(r.Context(), req.RefreshToken)
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	httpx.JSON(w, http.StatusOK, result)
}

// Logout handles POST /auth/logout, revoking the presented refresh token.
func (h *AuthHandler) Logout(w http.ResponseWriter, r *http.Request) {
	var req refreshRequest
	if err := httpx.DecodeJSON(w, r, &req); err != nil {
		httpx.Error(w, r, err)
		return
	}

	if err := h.auth.Logout(r.Context(), req.RefreshToken); err != nil {
		httpx.Error(w, r, err)
		return
	}
	httpx.NoContent(w)
}

// Me handles GET /auth/me, returning the caller's own account.
func (h *AuthHandler) Me(w http.ResponseWriter, r *http.Request) {
	userID, err := middleware.MustUserID(r.Context())
	if err != nil {
		httpx.Error(w, r, err)
		return
	}

	user, err := h.auth.CurrentUser(r.Context(), userID)
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	httpx.JSON(w, http.StatusOK, user)
}
