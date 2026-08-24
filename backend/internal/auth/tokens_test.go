package auth_test

import (
	"strings"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/Aryan-Jain06/seatsync/backend/internal/auth"
	"github.com/Aryan-Jain06/seatsync/backend/internal/models"
)

var testSecret = []byte("test-secret-value-for-signing-tokens")

func newIssuer(accessTTL time.Duration) *auth.TokenIssuer {
	return auth.NewTokenIssuer(testSecret, accessTTL, 24*time.Hour)
}

func TestAccessTokenRoundTrip(t *testing.T) {
	issuer := newIssuer(15 * time.Minute)
	userID := uuid.New()

	token, expiresAt, err := issuer.NewAccessToken(userID, models.RoleAdmin)
	require.NoError(t, err)
	require.NotEmpty(t, token)
	require.WithinDuration(t, time.Now().Add(15*time.Minute), expiresAt, 5*time.Second)

	claims, err := issuer.ParseAccessToken(token)
	require.NoError(t, err)

	gotID, err := claims.UserID()
	require.NoError(t, err)
	require.Equal(t, userID, gotID)
	require.Equal(t, models.RoleAdmin, claims.Role)
}

func TestExpiredAccessTokenIsRejected(t *testing.T) {
	// A negative TTL yields a token that expired before it was minted.
	issuer := newIssuer(-time.Minute)

	token, _, err := issuer.NewAccessToken(uuid.New(), models.RoleUser)
	require.NoError(t, err)

	_, err = issuer.ParseAccessToken(token)
	require.ErrorIs(t, err, auth.ErrTokenExpired)
}

func TestTokenSignedWithAnotherSecretIsRejected(t *testing.T) {
	token, _, err := newIssuer(time.Hour).NewAccessToken(uuid.New(), models.RoleUser)
	require.NoError(t, err)

	attacker := auth.NewTokenIssuer([]byte("a-different-secret"), time.Hour, time.Hour)

	_, err = attacker.ParseAccessToken(token)
	require.ErrorIs(t, err, auth.ErrTokenInvalid)
}

// TestNoneAlgorithmIsRejected covers the classic JWT downgrade: a token whose
// header claims alg=none must never be trusted, even though it parses.
func TestNoneAlgorithmIsRejected(t *testing.T) {
	unsigned := jwt.NewWithClaims(jwt.SigningMethodNone, auth.Claims{
		RegisteredClaims: jwt.RegisteredClaims{
			Subject:   uuid.NewString(),
			Issuer:    "seatsync",
			ExpiresAt: jwt.NewNumericDate(time.Now().Add(time.Hour)),
		},
		Role: models.RoleAdmin,
	})

	raw, err := unsigned.SignedString(jwt.UnsafeAllowNoneSignatureType)
	require.NoError(t, err)

	_, err = newIssuer(time.Hour).ParseAccessToken(raw)
	require.ErrorIs(t, err, auth.ErrTokenInvalid)
}

func TestTamperedTokenIsRejected(t *testing.T) {
	issuer := newIssuer(time.Hour)

	token, _, err := issuer.NewAccessToken(uuid.New(), models.RoleUser)
	require.NoError(t, err)

	// Flip a character in the payload segment; the signature no longer matches.
	parts := strings.Split(token, ".")
	require.Len(t, parts, 3)
	payload := []byte(parts[1])
	if payload[0] == 'e' {
		payload[0] = 'f'
	} else {
		payload[0] = 'e'
	}
	tampered := parts[0] + "." + string(payload) + "." + parts[2]

	_, err = issuer.ParseAccessToken(tampered)
	require.ErrorIs(t, err, auth.ErrTokenInvalid)
}

func TestRefreshTokensAreUniqueAndHashStably(t *testing.T) {
	issuer := newIssuer(time.Hour)

	first, firstHash, expiresAt, err := issuer.NewRefreshToken()
	require.NoError(t, err)
	require.NotEmpty(t, first)
	require.WithinDuration(t, time.Now().Add(24*time.Hour), expiresAt, 5*time.Second)

	second, secondHash, _, err := issuer.NewRefreshToken()
	require.NoError(t, err)

	require.NotEqual(t, first, second, "each refresh token must be unique")
	require.NotEqual(t, firstHash, secondHash)

	// The stored hash must be reproducible from the presented plaintext,
	// otherwise refresh lookups could never match.
	require.Equal(t, firstHash, auth.HashRefreshToken(first))
	require.Len(t, firstHash, 32, "sha-256 digest is 32 bytes")

	// The plaintext must not be recoverable from what is stored.
	require.NotContains(t, string(firstHash), first)
}

func TestGarbageTokenIsRejected(t *testing.T) {
	issuer := newIssuer(time.Hour)

	for _, raw := range []string{"", "not-a-jwt", "a.b.c", "....."} {
		_, err := issuer.ParseAccessToken(raw)
		require.Error(t, err, "expected %q to be rejected", raw)
	}
}
