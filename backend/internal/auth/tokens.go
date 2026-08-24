// Package auth mints and verifies the credentials the API issues: short-lived
// stateless JWT access tokens, and long-lived opaque refresh tokens whose
// hashes are stored in the database so they can be revoked.
package auth

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/google/uuid"

	"github.com/Aryan-Jain06/seatsync/backend/internal/models"
)

// Errors returned when a presented token cannot be trusted.
var (
	ErrTokenInvalid = errors.New("auth: token is invalid")
	ErrTokenExpired = errors.New("auth: token has expired")
)

const issuer = "seatsync"

// Claims is the payload carried by an access token.
type Claims struct {
	jwt.RegisteredClaims
	Role models.Role `json:"role"`
}

// UserID returns the subject as a UUID.
func (c *Claims) UserID() (uuid.UUID, error) {
	id, err := uuid.Parse(c.Subject)
	if err != nil {
		return uuid.Nil, fmt.Errorf("auth: token subject %q is not a uuid: %w", c.Subject, err)
	}
	return id, nil
}

// TokenIssuer mints and verifies tokens using a shared secret.
type TokenIssuer struct {
	secret     []byte
	accessTTL  time.Duration
	refreshTTL time.Duration
}

// NewTokenIssuer builds a TokenIssuer.
func NewTokenIssuer(secret []byte, accessTTL, refreshTTL time.Duration) *TokenIssuer {
	return &TokenIssuer{secret: secret, accessTTL: accessTTL, refreshTTL: refreshTTL}
}

// AccessTTL exposes the access token lifetime so handlers can report it.
func (t *TokenIssuer) AccessTTL() time.Duration { return t.accessTTL }

// RefreshTTL exposes the refresh token lifetime.
func (t *TokenIssuer) RefreshTTL() time.Duration { return t.refreshTTL }

// NewAccessToken signs an access token for the user.
func (t *TokenIssuer) NewAccessToken(userID uuid.UUID, role models.Role) (string, time.Time, error) {
	now := time.Now()
	expiresAt := now.Add(t.accessTTL)

	claims := Claims{
		RegisteredClaims: jwt.RegisteredClaims{
			Subject:   userID.String(),
			Issuer:    issuer,
			IssuedAt:  jwt.NewNumericDate(now),
			NotBefore: jwt.NewNumericDate(now),
			ExpiresAt: jwt.NewNumericDate(expiresAt),
			ID:        uuid.NewString(),
		},
		Role: role,
	}

	signed, err := jwt.NewWithClaims(jwt.SigningMethodHS256, claims).SignedString(t.secret)
	if err != nil {
		return "", time.Time{}, fmt.Errorf("auth: sign access token: %w", err)
	}
	return signed, expiresAt, nil
}

// ParseAccessToken verifies a token's signature, algorithm and expiry.
func (t *TokenIssuer) ParseAccessToken(raw string) (*Claims, error) {
	claims := &Claims{}

	_, err := jwt.ParseWithClaims(raw, claims, func(token *jwt.Token) (any, error) {
		// Pin the algorithm. Without this check a token signed with "none",
		// or with an asymmetric algorithm, could be accepted.
		if _, ok := token.Method.(*jwt.SigningMethodHMAC); !ok {
			return nil, fmt.Errorf("unexpected signing method %v", token.Header["alg"])
		}
		return t.secret, nil
	},
		jwt.WithValidMethods([]string{jwt.SigningMethodHS256.Alg()}),
		jwt.WithIssuer(issuer),
	)
	if err != nil {
		if errors.Is(err, jwt.ErrTokenExpired) {
			return nil, ErrTokenExpired
		}
		return nil, fmt.Errorf("%w: %v", ErrTokenInvalid, err)
	}
	return claims, nil
}

// refreshTokenBytes is the entropy behind an opaque refresh token.
const refreshTokenBytes = 32

// NewRefreshToken returns a fresh opaque token, the hash to persist, and its
// expiry. The plaintext is shown to the client exactly once; only the hash is
// stored, so a database leak does not yield usable sessions.
func (t *TokenIssuer) NewRefreshToken() (token string, hash []byte, expiresAt time.Time, err error) {
	buf := make([]byte, refreshTokenBytes)
	if _, err := rand.Read(buf); err != nil {
		return "", nil, time.Time{}, fmt.Errorf("auth: generate refresh token: %w", err)
	}

	token = base64.RawURLEncoding.EncodeToString(buf)
	return token, HashRefreshToken(token), time.Now().Add(t.refreshTTL), nil
}

// HashRefreshToken derives the stored form of a refresh token.
//
// SHA-256 without a salt is deliberate: the input is 256 bits of uniform
// randomness, so it is not guessable by brute force the way a password is,
// and an unsalted digest keeps lookup a single indexed query.
func HashRefreshToken(token string) []byte {
	sum := sha256.Sum256([]byte(token))
	return sum[:]
}
