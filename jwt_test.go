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

	parser := elephantine.NewStaticAuthInfoParser(jwtKey.PublicKey, elephantine.JWTAuthInfoParserOptions{})
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

	parser := elephantine.NewStaticAuthInfoParser(jwtKey.PublicKey, elephantine.JWTAuthInfoParserOptions{
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

	parser := elephantine.NewStaticAuthInfoParser(jwtKey.PublicKey, elephantine.JWTAuthInfoParserOptions{})

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

	parser := elephantine.NewStaticAuthInfoParser(jwtKey.PublicKey, elephantine.JWTAuthInfoParserOptions{
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

	parser := elephantine.NewStaticAuthInfoParser(jwtKey.PublicKey, elephantine.JWTAuthInfoParserOptions{
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

	parser := elephantine.NewStaticAuthInfoParser(jwtKey.PublicKey, elephantine.JWTAuthInfoParserOptions{})
	token := jwt.NewWithClaims(jwt.SigningMethodES384, elephantine.JWTClaims{
		Units: []string{
			"external://resource/thing",
			"/unqualified-name",
			"core://unit/fqn",
			"with-params?id=123#and-hash",
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
		"core://unit/with-params?id=123#and-hash",
	}, info.Claims.Units, "get the expected units")
}

func TestAuthInfoSubjectMapping(t *testing.T) {
	jwtKey, err := ecdsa.GenerateKey(elliptic.P384(), rand.Reader)
	test.Must(t, err, "create signing key")

	cases := map[string]elephantine.JWTClaims{
		"core://user/7b328bf3-a53b-4024-a895-c68cb14fdd97": {
			RegisteredClaims: jwt.RegisteredClaims{
				Subject: "7b328bf3-a53b-4024-a895-c68cb14fdd97",
			},
		},
		"core://user/df1a0b4f-9483-4639-9143-417e876a3405": {
			RegisteredClaims: jwt.RegisteredClaims{
				Subject: "core://user/df1a0b4f-9483-4639-9143-417e876a3405",
			},
		},
		"core://application/name-of-app": {
			RegisteredClaims: jwt.RegisteredClaims{
				Subject: "17c11ca5-1eea-4e31-a31c-a0c6e937abd0",
			},
			AuthorizedParty: "name-of-app",
		},
		"external://sub/of/some/kind": {
			RegisteredClaims: jwt.RegisteredClaims{
				Subject: "external://sub/of/some/kind",
			},
		},
	}

	parser := elephantine.NewStaticAuthInfoParser(
		jwtKey.PublicKey, elephantine.JWTAuthInfoParserOptions{},
	)

	for want, input := range cases {
		token := jwt.NewWithClaims(jwt.SigningMethodES384, input)

		ss, err := token.SignedString(jwtKey)
		test.Must(t, err, "sign JWT token")

		info, err := parser.AuthInfoFromHeader(
			fmt.Sprintf("Bearer %s", ss))
		test.Must(t, err, "parse token")

		test.Equal(t, want, info.Claims.Subject,
			"get the expected sub")
		test.Equal(t, input.Subject, info.Claims.OriginalSub,
			"preserve original sub")
	}
}
