package elephantine_test

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"fmt"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/ttab/elephantine"
	"github.com/ttab/elephantine/test"
)

func TestHandleTokenWithoutExpiry(t *testing.T) {
	jwtKey, err := ecdsa.GenerateKey(elliptic.P384(), rand.Reader)
	test.Must(t, err, "create signing key")

	parser := elephantine.NewStaticAuthInfoParser(jwtKey.PublicKey, elephantine.AuthInfoParserOptions{})
	token := jwt.NewWithClaims(jwt.SigningMethodES384, elephantine.JWTClaims{
		RegisteredClaims: jwt.RegisteredClaims{
			Issuer: "test",
		},
	})

	ss, err := token.SignedString(jwtKey)
	test.Must(t, err, "sign JWT token")

	_, err = parser.AuthInfoFromHeader(fmt.Sprintf("Bearer %s", ss))
	test.Must(t, err, "parse token")
}

func TestVerifyIssuer(t *testing.T) {
	jwtKey, err := ecdsa.GenerateKey(elliptic.P384(), rand.Reader)
	test.Must(t, err, "create signing key")

	parser := elephantine.NewStaticAuthInfoParser(jwtKey.PublicKey, elephantine.AuthInfoParserOptions{
		Issuer: "test",
	})
	token := jwt.NewWithClaims(jwt.SigningMethodES384, elephantine.JWTClaims{
		RegisteredClaims: jwt.RegisteredClaims{
			Issuer: "myrandomissuer",
		},
	})

	ss, err := token.SignedString(jwtKey)
	test.Must(t, err, "sign JWT token")

	_, err = parser.AuthInfoFromHeader(fmt.Sprintf("Bearer %s", ss))
	test.MustNot(t, err, "validate token with wrong issuer")
}

func TestVerifyExpiry(t *testing.T) {
	jwtKey, err := ecdsa.GenerateKey(elliptic.P384(), rand.Reader)
	test.Must(t, err, "create signing key")

	parser := elephantine.NewStaticAuthInfoParser(jwtKey.PublicKey, elephantine.AuthInfoParserOptions{})

	token := jwt.NewWithClaims(jwt.SigningMethodES384, elephantine.JWTClaims{
		RegisteredClaims: jwt.RegisteredClaims{
			ExpiresAt: jwt.NewNumericDate(time.Now().Add(-10 * time.Second)),
		},
	})

	ss, err := token.SignedString(jwtKey)
	test.Must(t, err, "sign JWT token")

	_, err = parser.AuthInfoFromHeader(fmt.Sprintf("Bearer %s", ss))
	test.MustNot(t, err, "validate expired token")
}

func TestAuthInfoParsesScopes(t *testing.T) {
	jwtKey, err := ecdsa.GenerateKey(elliptic.P384(), rand.Reader)
	test.Must(t, err, "create signing key")

	parser := elephantine.NewStaticAuthInfoParser(jwtKey.PublicKey, elephantine.AuthInfoParserOptions{
		Issuer: "test",
	})
	token := jwt.NewWithClaims(jwt.SigningMethodES384, elephantine.JWTClaims{
		RegisteredClaims: jwt.RegisteredClaims{
			Issuer: "test",
		},
		Name:  "jolifanto",
		Scope: "doc_read doc_write",
	})

	ss, err := token.SignedString(jwtKey)
	test.Must(t, err, "sign JWT token")

	info, err := parser.AuthInfoFromHeader(fmt.Sprintf("Bearer %s", ss))
	test.Must(t, err, "parse token")

	test.Equal(t, "doc_read doc_write", info.Claims.Scope, "preserves scope")
}

func TestAuthInfoStripsScopePrefix(t *testing.T) {
	jwtKey, err := ecdsa.GenerateKey(elliptic.P384(), rand.Reader)
	test.Must(t, err, "create signing key")

	parser := elephantine.NewStaticAuthInfoParser(jwtKey.PublicKey, elephantine.AuthInfoParserOptions{
		ScopePrefix: "test_",
	})
	token := jwt.NewWithClaims(jwt.SigningMethodES384, elephantine.JWTClaims{
		Scope: "test_doc_read test_doc_write",
	})

	ss, err := token.SignedString(jwtKey)
	test.Must(t, err, "sign JWT token")

	info, err := parser.AuthInfoFromHeader(fmt.Sprintf("Bearer %s", ss))
	test.Must(t, err, "parse token")

	test.Equal(t, "doc_read doc_write", info.Claims.Scope, "strip scope prefix")
}

func TestAuthInfoUnitMapping(t *testing.T) {
	jwtKey, err := ecdsa.GenerateKey(elliptic.P384(), rand.Reader)
	test.Must(t, err, "create signing key")

	parser := elephantine.NewStaticAuthInfoParser(jwtKey.PublicKey, elephantine.AuthInfoParserOptions{})
	token := jwt.NewWithClaims(jwt.SigningMethodES384, elephantine.JWTClaims{
		Units: []string{
			"external://resource/thing",
			"/unqualified-name",
			"core://unit/fqn",
		},
	})

	ss, err := token.SignedString(jwtKey)
	test.Must(t, err, "sign JWT token")

	info, err := parser.AuthInfoFromHeader(fmt.Sprintf("Bearer %s", ss))
	test.Must(t, err, "parse token")

	test.EqualDiff(t, []string{
		"external://resource/thing",
		"core://unit/unqualified-name",
		"core://unit/fqn",
	}, info.Claims.Units, "get the expected units")
}
