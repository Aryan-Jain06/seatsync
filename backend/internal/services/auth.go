// Package services holds the business logic that sits between HTTP handlers
// and the data layer.
package services

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/mail"

	"github.com/google/uuid"
	"strings"
	"time"
	"unicode/utf8"

	"golang.org/x/crypto/bcrypt"

	"github.com/Aryan-Jain06/seatsync/backend/internal/auth"
	"github.com/Aryan-Jain06/seatsync/backend/internal/httpx"
	"github.com/Aryan-Jain06/seatsync/backend/internal/models"
	"github.com/Aryan-Jain06/seatsync/backend/internal/repos"
)

// Password policy. The upper bound exists because bcrypt silently truncates
// input beyond 72 bytes, which would make two different long passwords
// interchangeable.
const (
	minPasswordLength = 8
	maxPasswordBytes  = 72
	maxNameLength     = 100
	maxEmailLength    = 254
)

// AuthService implements registration, login and refresh-token rotation.
type AuthService struct {
	users  *repos.UserRepo
	tokens *repos.RefreshTokenRepo
	issuer *auth.TokenIssuer
	bcrypt int
}

// NewAuthService builds an AuthService. bcryptCost of 0 selects the library
// default; tests lower it so they do not spend seconds hashing.
func NewAuthService(users *repos.UserRepo, tokens *repos.RefreshTokenRepo, issuer *auth.TokenIssuer, bcryptCost int) *AuthService {
	if bcryptCost == 0 {
		bcryptCost = bcrypt.DefaultCost
	}
	return &AuthService{users: users, tokens: tokens, issuer: issuer, bcrypt: bcryptCost}
}

// TokenPair is what the auth endpoints hand back to a client.
type TokenPair struct {
	AccessToken  string    `json:"access_token"`
	RefreshToken string    `json:"refresh_token"`
	ExpiresAt    time.Time `json:"expires_at"`
	TokenType    string    `json:"token_type"`
}

// AuthResult couples the authenticated user with fresh credentials.
type AuthResult struct {
	User   *models.User `json:"user"`
	Tokens TokenPair    `json:"tokens"`
}

// Register creates an account and signs the new user in.
func (s *AuthService) Register(ctx context.Context, email, password, name string) (*AuthResult, error) {
	email = strings.TrimSpace(email)
	name = strings.TrimSpace(name)

	if err := validateEmail(email); err != nil {
		return nil, err
	}
	if err := validatePassword(password); err != nil {
		return nil, err
	}
	if name == "" {
		return nil, httpx.Validation("Name is required.")
	}
	if utf8.RuneCountInString(name) > maxNameLength {
		return nil, httpx.Validation(fmt.Sprintf("Name must be at most %d characters.", maxNameLength))
	}

	hash, err := bcrypt.GenerateFromPassword([]byte(password), s.bcrypt)
	if err != nil {
		return nil, httpx.Internal(fmt.Errorf("hash password: %w", err))
	}

	user, err := s.users.Create(ctx, email, string(hash), name, models.RoleUser)
	if err != nil {
		if errors.Is(err, repos.ErrDuplicateKey) {
			return nil, httpx.Conflict(httpx.CodeConflict, "An account with that email already exists.")
		}
		return nil, httpx.Internal(fmt.Errorf("create user: %w", err))
	}

	tokens, err := s.issueTokens(ctx, user)
	if err != nil {
		return nil, err
	}
	return &AuthResult{User: user, Tokens: *tokens}, nil
}

// Login exchanges credentials for tokens.
func (s *AuthService) Login(ctx context.Context, email, password string) (*AuthResult, error) {
	email = strings.TrimSpace(email)
	if email == "" || password == "" {
		return nil, httpx.Unauthorized("Email or password is incorrect.")
	}

	user, err := s.users.GetByEmail(ctx, email)
	if err != nil {
		if errors.Is(err, repos.ErrNotFound) {
			// Spend the cost of a hash comparison anyway. Returning early for
			// unknown emails would let an attacker distinguish registered
			// addresses by timing alone.
			bcrypt.CompareHashAndPassword(dummyHash, []byte(password)) //nolint:errcheck // timing equalisation only
			return nil, httpx.Unauthorized("Email or password is incorrect.")
		}
		return nil, httpx.Internal(fmt.Errorf("look up user: %w", err))
	}

	if err := bcrypt.CompareHashAndPassword([]byte(user.PasswordHash), []byte(password)); err != nil {
		return nil, httpx.Unauthorized("Email or password is incorrect.")
	}

	tokens, err := s.issueTokens(ctx, user)
	if err != nil {
		return nil, err
	}
	return &AuthResult{User: user, Tokens: *tokens}, nil
}

// Refresh rotates a refresh token: the presented token is revoked and a new
// pair is issued. Presenting an already-revoked token is treated as possible
// theft and ends every session for that user.
func (s *AuthService) Refresh(ctx context.Context, refreshToken string) (*AuthResult, error) {
	refreshToken = strings.TrimSpace(refreshToken)
	if refreshToken == "" {
		return nil, httpx.Unauthorized("Refresh token is required.")
	}

	stored, err := s.tokens.GetByHash(ctx, auth.HashRefreshToken(refreshToken))
	if err != nil {
		if errors.Is(err, repos.ErrNotFound) {
			return nil, httpx.Unauthorized("Refresh token is invalid or has expired.")
		}
		return nil, httpx.Internal(fmt.Errorf("look up refresh token: %w", err))
	}

	if stored.RevokedAt != nil {
		// The token was already spent. Either it was replayed by an attacker
		// or a legitimate client raced with itself; either way the safe move
		// is to invalidate the whole family.
		slog.WarnContext(ctx, "revoked refresh token presented; revoking all sessions for user",
			"user_id", stored.UserID)
		if err := s.tokens.RevokeAllForUser(ctx, stored.UserID); err != nil {
			slog.ErrorContext(ctx, "failed to revoke user sessions", "error", err, "user_id", stored.UserID)
		}
		return nil, httpx.Unauthorized("Refresh token is invalid or has expired.")
	}

	if !stored.Active() {
		return nil, httpx.Unauthorized("Refresh token is invalid or has expired.")
	}

	user, err := s.users.GetByID(ctx, stored.UserID)
	if err != nil {
		if errors.Is(err, repos.ErrNotFound) {
			return nil, httpx.Unauthorized("Refresh token is invalid or has expired.")
		}
		return nil, httpx.Internal(fmt.Errorf("load user for refresh: %w", err))
	}

	if err := s.tokens.Revoke(ctx, stored.ID); err != nil {
		return nil, httpx.Internal(fmt.Errorf("revoke rotated token: %w", err))
	}

	tokens, err := s.issueTokens(ctx, user)
	if err != nil {
		return nil, err
	}
	return &AuthResult{User: user, Tokens: *tokens}, nil
}

// Logout revokes a single refresh token. An unknown token is not an error:
// the caller's intent, that the session should not work any more, is satisfied.
func (s *AuthService) Logout(ctx context.Context, refreshToken string) error {
	if strings.TrimSpace(refreshToken) == "" {
		return nil
	}

	stored, err := s.tokens.GetByHash(ctx, auth.HashRefreshToken(refreshToken))
	if err != nil {
		if errors.Is(err, repos.ErrNotFound) {
			return nil
		}
		return httpx.Internal(fmt.Errorf("look up refresh token: %w", err))
	}

	if err := s.tokens.Revoke(ctx, stored.ID); err != nil {
		return httpx.Internal(fmt.Errorf("revoke refresh token: %w", err))
	}
	return nil
}

// issueTokens mints an access/refresh pair and records the refresh hash.
func (s *AuthService) issueTokens(ctx context.Context, user *models.User) (*TokenPair, error) {
	accessToken, expiresAt, err := s.issuer.NewAccessToken(user.ID, user.Role)
	if err != nil {
		return nil, httpx.Internal(err)
	}

	refreshToken, hash, refreshExpiry, err := s.issuer.NewRefreshToken()
	if err != nil {
		return nil, httpx.Internal(err)
	}

	if err := s.tokens.Create(ctx, user.ID, hash, refreshExpiry); err != nil {
		return nil, httpx.Internal(fmt.Errorf("store refresh token: %w", err))
	}

	return &TokenPair{
		AccessToken:  accessToken,
		RefreshToken: refreshToken,
		ExpiresAt:    expiresAt,
		TokenType:    "Bearer",
	}, nil
}

// dummyHash is a valid bcrypt digest of an unguessable value, compared against
// when the email is unknown so login takes the same time either way.
var dummyHash = []byte("$2a$10$N9qo8uLOickgx2ZMRZoMyeIjZAgcfl7p92ldGxad68LJZdL17lhWy")

func validateEmail(email string) error {
	if email == "" {
		return httpx.Validation("Email is required.")
	}
	if len(email) > maxEmailLength {
		return httpx.Validation(fmt.Sprintf("Email must be at most %d characters.", maxEmailLength))
	}
	addr, err := mail.ParseAddress(email)
	if err != nil || addr.Address != email {
		return httpx.Validation("Email is not a valid address.")
	}
	return nil
}

func validatePassword(password string) error {
	if password == "" {
		return httpx.Validation("Password is required.")
	}
	if utf8.RuneCountInString(password) < minPasswordLength {
		return httpx.Validation(fmt.Sprintf("Password must be at least %d characters.", minPasswordLength))
	}
	// bcrypt only reads the first 72 bytes, so reject longer input outright
	// rather than accepting a password whose tail is silently ignored.
	if len(password) > maxPasswordBytes {
		return httpx.Validation(fmt.Sprintf("Password must be at most %d bytes.", maxPasswordBytes))
	}
	return nil
}

// CurrentUser loads the signed-in user's account.
func (s *AuthService) CurrentUser(ctx context.Context, userID uuid.UUID) (*models.User, error) {
	user, err := s.users.GetByID(ctx, userID)
	if err != nil {
		if errors.Is(err, repos.ErrNotFound) {
			return nil, httpx.NotFound("Account not found.")
		}
		return nil, httpx.Internal(fmt.Errorf("load current user: %w", err))
	}
	return user, nil
}
