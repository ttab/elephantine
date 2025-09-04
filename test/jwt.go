package test

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"strings"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/ttab/elephantine"
)

// NewSigningKey creates a signing key for use with a
// elephantine.NewStaticAuthInfoParser.
func NewSigningKey(t *testing.T) *ecdsa.PrivateKey {
	jwtKey, err := ecdsa.GenerateKey(elliptic.P384(), rand.Reader)
	Must(t, err, "generate signing key")

	return jwtKey
}

// Claims creates an elephantine.JWTClaims struct for use in thesting.
func Claims(
	t *testing.T, user string, scope string, units ...string,
) elephantine.JWTClaims {
	t.Helper()

	return elephantine.JWTClaims{
		RegisteredClaims: jwt.RegisteredClaims{
			ExpiresAt: jwt.NewNumericDate(
				time.Now().Add(10 * time.Minute)),
			Issuer:  "test",
			Subject: "user://test/" + user,
		},
		Name:  t.Name(),
		Units: units,
		Scope: scope,
	}
}

// StandardClaims is a helper function that creates standard claims for a test.
func StandardClaims(
	t *testing.T, scope string, units ...string,
) elephantine.JWTClaims {
	t.Helper()

	return Claims(t, strings.ToLower(t.Name()), scope, units...)
}

// AccessKey creates a signed access key from the signing key and claims.
func AccessKey(
	t *testing.T, key *ecdsa.PrivateKey, claims elephantine.JWTClaims,
) string {
	t.Helper()

	token := jwt.NewWithClaims(jwt.SigningMethodES384, claims)

	ss, err := token.SignedString(key)
	Must(t, err, "sign JWT token")

	return ss
}
