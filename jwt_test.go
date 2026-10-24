package elephantine_test

import (
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/elliptic"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/ttab/elephantine"
	"github.com/ttab/elephantine/test"
)

const testIssuer = "test"

func TestHandleTokenWithoutExpiry(t *testing.T) {
	jwtKey, err := ecdsa.GenerateKey(elliptic.P384(), rand.Reader)
	test.Mustf(t, err, "create signing key")

	parser := elephantine.NewStaticAuthInfoParser(
		t.Context(),
		jwtKey.PublicKey,
		elephantine.JWTAuthInfoParserOptions{},
	)

	token := jwt.NewWithClaims(jwt.SigningMethodES384, elephantine.JWTClaims{
		RegisteredClaims: jwt.RegisteredClaims{
			Issuer: testIssuer,
		},
	})

	ss, err := token.SignedString(jwtKey)
	test.Mustf(t, err, "sign JWT token")

	_, err = parser.AuthInfoFromHeader(fmt.Sprintf("Bearer %s", ss))
	test.Mustf(t, err, "parse token")
}

func TestVerifyIssuer(t *testing.T) {
	jwtKey, err := ecdsa.GenerateKey(elliptic.P384(), rand.Reader)
	test.Mustf(t, err, "create signing key")

	parser := elephantine.NewStaticAuthInfoParser(
		t.Context(),
		jwtKey.PublicKey,
		elephantine.JWTAuthInfoParserOptions{
			Issuer: testIssuer,
		},
	)

	token := jwt.NewWithClaims(jwt.SigningMethodES384, elephantine.JWTClaims{
		RegisteredClaims: jwt.RegisteredClaims{
			Issuer: "myrandomissuer",
		},
	})

	ss, err := token.SignedString(jwtKey)
	test.Mustf(t, err, "sign JWT token")

	_, err = parser.AuthInfoFromHeader(fmt.Sprintf("Bearer %s", ss))
	test.MustNotf(t, err, "validate token with wrong issuer")
}

func TestVerifyExpiry(t *testing.T) {
	jwtKey, err := ecdsa.GenerateKey(elliptic.P384(), rand.Reader)
	test.Mustf(t, err, "create signing key")

	parser := elephantine.NewStaticAuthInfoParser(
		t.Context(),
		jwtKey.PublicKey,
		elephantine.JWTAuthInfoParserOptions{},
	)

	token := jwt.NewWithClaims(jwt.SigningMethodES384, elephantine.JWTClaims{
		RegisteredClaims: jwt.RegisteredClaims{
			ExpiresAt: jwt.NewNumericDate(time.Now().Add(-10 * time.Second)),
		},
	})

	ss, err := token.SignedString(jwtKey)
	test.Mustf(t, err, "sign JWT token")

	_, err = parser.AuthInfoFromHeader(fmt.Sprintf("Bearer %s", ss))
	test.MustNotf(t, err, "validate expired token")
}

func TestAuthInfoParsesScopes(t *testing.T) {
	jwtKey, err := ecdsa.GenerateKey(elliptic.P384(), rand.Reader)
	test.Mustf(t, err, "create signing key")

	parser := elephantine.NewStaticAuthInfoParser(
		t.Context(),
		jwtKey.PublicKey,
		elephantine.JWTAuthInfoParserOptions{
			Issuer: testIssuer,
		},
	)

	token := jwt.NewWithClaims(jwt.SigningMethodES384, elephantine.JWTClaims{
		RegisteredClaims: jwt.RegisteredClaims{
			Issuer: testIssuer,
		},
		Name:  "jolifanto",
		Email: "jolifanto@example.com",
		Scope: "doc_read doc_write",
	})

	ss, err := token.SignedString(jwtKey)
	test.Mustf(t, err, "sign JWT token")

	info, err := parser.AuthInfoFromHeader(fmt.Sprintf("Bearer %s", ss))
	test.Mustf(t, err, "parse token")

	test.Equalf(t, "doc_read doc_write", info.Claims.Scope, "preserves scope")
	test.Equalf(t, "jolifanto@example.com", info.Claims.Email, "preserves email")
}

func TestAuthInfoStripsScopePrefix(t *testing.T) {
	jwtKey, err := ecdsa.GenerateKey(elliptic.P384(), rand.Reader)
	test.Mustf(t, err, "create signing key")

	parser := elephantine.NewStaticAuthInfoParser(
		t.Context(),
		jwtKey.PublicKey,
		elephantine.JWTAuthInfoParserOptions{
			ScopePrefix: "test_",
		},
	)

	token := jwt.NewWithClaims(jwt.SigningMethodES384, elephantine.JWTClaims{
		Scope: "test_doc_read test_doc_write",
	})

	ss, err := token.SignedString(jwtKey)
	test.Mustf(t, err, "sign JWT token")

	info, err := parser.AuthInfoFromHeader(fmt.Sprintf("Bearer %s", ss))
	test.Mustf(t, err, "parse token")

	test.Equalf(t, "doc_read doc_write", info.Claims.Scope, "strip scope prefix")
}

func TestAuthInfoUnitMapping(t *testing.T) {
	jwtKey, err := ecdsa.GenerateKey(elliptic.P384(), rand.Reader)
	test.Mustf(t, err, "create signing key")

	parser := elephantine.NewStaticAuthInfoParser(
		t.Context(),
		jwtKey.PublicKey,
		elephantine.JWTAuthInfoParserOptions{},
	)

	token := jwt.NewWithClaims(jwt.SigningMethodES384, elephantine.JWTClaims{
		Units: []string{
			"external://resource/thing",
			"/unqualified-name",
			"core://unit/fqn",
			"with-params?id=123#and-hash",
		},
	})

	ss, err := token.SignedString(jwtKey)
	test.Mustf(t, err, "sign JWT token")

	info, err := parser.AuthInfoFromHeader(fmt.Sprintf("Bearer %s", ss))
	test.Mustf(t, err, "parse token")

	test.EqualDiff(t, []string{
		"external://resource/thing",
		"core://unit/unqualified-name",
		"core://unit/fqn",
		"core://unit/with-params?id=123#and-hash",
	}, info.Claims.Units, "get the expected units")
}

func TestAuthInfoSubjectMapping(t *testing.T) {
	jwtKey, err := ecdsa.GenerateKey(elliptic.P384(), rand.Reader)
	test.Mustf(t, err, "create signing key")

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
			ClientID: "name-of-app",
		},
		"external://sub/of/some/kind": {
			RegisteredClaims: jwt.RegisteredClaims{
				Subject: "external://sub/of/some/kind",
			},
		},
	}

	parser := elephantine.NewStaticAuthInfoParser(
		t.Context(),
		jwtKey.PublicKey,
		elephantine.JWTAuthInfoParserOptions{},
	)

	for want, input := range cases {
		token := jwt.NewWithClaims(jwt.SigningMethodES384, input)

		ss, err := token.SignedString(jwtKey)
		test.Mustf(t, err, "sign JWT token")

		info, err := parser.AuthInfoFromHeader(
			fmt.Sprintf("Bearer %s", ss))
		test.Mustf(t, err, "parse token")

		test.Equalf(t, want, info.Claims.Subject,
			"get the expected sub")
		test.Equalf(t, input.Subject, info.Claims.OriginalSub,
			"preserve original sub")
	}
}

// The delegate issuer and the audience the delegate tests accept, and the scope
// prefix they strip.
const (
	testDelegateIssuer   = "elephant-delegate"
	testDelegateAudience = "elephant-stream"
	testScopePrefix      = "test_"
)

func TestStripScopePrefixOnlyAtScopeStart(t *testing.T) {
	jwtKey, err := ecdsa.GenerateKey(elliptic.P384(), rand.Reader)
	test.Mustf(t, err, "create signing key")

	parser := elephantine.NewStaticAuthInfoParser(
		t.Context(),
		jwtKey.PublicKey,
		elephantine.JWTAuthInfoParserOptions{
			ScopePrefix: testScopePrefix,
		},
	)

	token := jwt.NewWithClaims(jwt.SigningMethodES384, elephantine.JWTClaims{
		Scope: "test_doc_read:test_x test_doc_write x-test_y",
	})

	ss, err := token.SignedString(jwtKey)
	test.Mustf(t, err, "sign JWT token")

	info, err := parser.AuthInfoFromHeader("Bearer " + ss)
	test.Mustf(t, err, "parse token")

	test.Equalf(t, "doc_read:test_x doc_write x-test_y", info.Claims.Scope,
		"strip the prefix from the start of each scope only")
}

func TestExactScopeMatching(t *testing.T) {
	claims := elephantine.JWTClaims{
		Scope: "doc_read:5b4b0f2c-6d0e-4b8e-9c55-0b9b8f1f3f4e  stream",
	}

	test.Equalf(t, false, claims.HasScope("doc_read"),
		"a suffixed scope does not satisfy the plain one")
	test.Equalf(t, false, claims.HasAnyScope("doc_read", "doc_write"),
		"a suffixed scope does not satisfy any plain one")
	test.Equalf(t, true,
		claims.HasScope("doc_read:5b4b0f2c-6d0e-4b8e-9c55-0b9b8f1f3f4e"),
		"the suffixed scope satisfies itself")
	test.Equalf(t, true, claims.HasScope("stream"),
		"match a scope after a double space")
	test.Equalf(t, false, claims.HasScope(""),
		"never match the empty scope")
}

// delegateKey creates an elephant-delegate stand-in: an Ed25519 key and the
// trusted issuer that verifies with it.
func delegateKey(
	t *testing.T, audience string,
) (ed25519.PrivateKey, elephantine.TrustedIssuer) {
	t.Helper()

	pub, key, err := ed25519.GenerateKey(rand.Reader)
	test.Mustf(t, err, "create the delegate key")

	return key, elephantine.TrustedIssuer{
		Issuer:   testDelegateIssuer,
		Audience: audience,
		Keyfunc: func(_ *jwt.Token) (any, error) {
			return pub, nil
		},
	}
}

func delegateClaims(aud string) elephantine.JWTClaims {
	return elephantine.JWTClaims{
		RegisteredClaims: jwt.RegisteredClaims{
			Issuer:    testDelegateIssuer,
			Subject:   "7b328bf3-a53b-4024-a895-c68cb14fdd97",
			Audience:  jwt.ClaimStrings{aud},
			ExpiresAt: jwt.NewNumericDate(time.Now().Add(time.Hour)),
		},
		Scope: "test_stream",
		Confirmation: &elephantine.Confirmation{
			JKT: "0ZcOCORZNYy-DWpqq30jZyJGHTN0d2HglBV3uiguA4I",
		},
		DocumentUUID: "5b4b0f2c-6d0e-4b8e-9c55-0b9b8f1f3f4e",
	}
}

func TestTrustedIssuer(t *testing.T) {
	idpKey, err := ecdsa.GenerateKey(elliptic.P384(), rand.Reader)
	test.Mustf(t, err, "create signing key")

	key, delegate := delegateKey(t, "")

	parser := elephantine.NewStaticAuthInfoParser(
		t.Context(),
		idpKey.PublicKey,
		elephantine.JWTAuthInfoParserOptions{
			Issuer:         testIssuer,
			Audience:       testDelegateAudience,
			ScopePrefix:    testScopePrefix,
			TrustedIssuers: []elephantine.TrustedIssuer{delegate},
		},
	)

	sign := func(claims elephantine.JWTClaims) string {
		ss, err := jwt.NewWithClaims(jwt.SigningMethodEdDSA, claims).
			SignedString(key)
		test.Mustf(t, err, "sign the delegate token")

		return ss
	}

	info, err := parser.AuthInfoFromHeader(
		"DPoP " + sign(delegateClaims(testDelegateAudience)))
	test.Mustf(t, err, "accept a delegate token for this audience")

	test.Equalf(t, "core://user/7b328bf3-a53b-4024-a895-c68cb14fdd97",
		info.Claims.Subject, "map the subject as for a session token")
	test.Equalf(t, "stream", info.Claims.Scope, "strip the scope prefix")
	test.Equalf(t, "5b4b0f2c-6d0e-4b8e-9c55-0b9b8f1f3f4e",
		info.Claims.DocumentUUID, "read the document binding")

	jkt, bound := info.Claims.BoundKey()
	test.Equalf(t, true, bound, "see the key binding")
	test.Equalf(t, "0ZcOCORZNYy-DWpqq30jZyJGHTN0d2HglBV3uiguA4I", jkt,
		"read the bound key")

	_, ok := elephantine.GetAuthInfo(
		elephantine.SetAuthInfo(t.Context(), info))
	test.Equalf(t, false, ok,
		"hide the bound token until its proof is verified")

	_, err = parser.AuthInfoFromHeader(
		"Bearer " + sign(delegateClaims("elephant-push")))
	test.MustNotf(t, err, "refuse a delegate token for another audience")

	noExpiry := delegateClaims(testDelegateAudience)
	noExpiry.ExpiresAt = nil

	_, err = parser.AuthInfoFromHeader("Bearer " + sign(noExpiry))
	test.MustNotf(t, err, "refuse a delegate token without exp")

	otherCnf := delegateClaims(testDelegateAudience)
	otherCnf.Confirmation = nil

	unbound := sign(otherCnf)

	_, err = parser.AuthInfoFromHeader("DPoP " + unbound)
	test.MustNotf(t, err, "refuse the DPoP scheme for an unbound token")

	_, err = parser.AuthInfoFromHeader("Bearer " + unbound)
	test.Mustf(t, err, "accept an unbound delegate token as a bearer token")

	mtls := jwt.MapClaims{
		"iss": testDelegateIssuer,
		"sub": "someone",
		"aud": testDelegateAudience,
		"exp": time.Now().Add(time.Hour).Unix(),
		"cnf": map[string]any{"x5t#S256": "abc"},
	}

	ss, err := jwt.NewWithClaims(jwt.SigningMethodEdDSA, mtls).
		SignedString(key)
	test.Mustf(t, err, "sign the token")

	_, err = parser.AuthInfoFromHeader("Bearer " + ss)
	test.MustNotf(t, err,
		"refuse a confirmation method that cannot be checked")

	primary := delegateClaims(testDelegateAudience)
	primary.Issuer = testIssuer

	_, err = parser.AuthInfoFromHeader("Bearer " + sign(primary))
	test.MustNotf(t, err,
		"refuse a delegate-signed token that claims the primary issuer")

	session := test.Claims(t, "hugo", "test_doc_read")
	session.Audience = jwt.ClaimStrings{testDelegateAudience}

	_, err = parser.AuthInfoFromHeader(
		"Bearer " + test.AccessKey(t, idpKey, session))
	test.Mustf(t, err, "accept a session token")
}

func TestTrustedIssuerWithoutAudience(t *testing.T) {
	idpKey, err := ecdsa.GenerateKey(elliptic.P384(), rand.Reader)
	test.Mustf(t, err, "create signing key")

	key, delegate := delegateKey(t, "")

	// Neither the parser nor the trusted issuer has an audience, so a
	// token minted for any service would be accepted: refuse them all.
	parser := elephantine.NewStaticAuthInfoParser(
		t.Context(),
		idpKey.PublicKey,
		elephantine.JWTAuthInfoParserOptions{
			Issuer:         testIssuer,
			TrustedIssuers: []elephantine.TrustedIssuer{delegate},
		},
	)

	ss, err := jwt.NewWithClaims(jwt.SigningMethodEdDSA,
		delegateClaims(testDelegateAudience)).SignedString(key)
	test.Mustf(t, err, "sign the delegate token")

	_, err = parser.AuthInfoFromHeader("Bearer " + ss)
	test.MustNotf(t, err, "refuse a token from an issuer with no audience")
}

func TestAuthenticationConfigDelegate(t *testing.T) {
	pub, key, err := ed25519.GenerateKey(rand.Reader)
	test.Mustf(t, err, "create the delegate key")

	mux := http.NewServeMux()
	srv := httptest.NewServer(mux)

	t.Cleanup(srv.Close)

	mux.HandleFunc("/.well-known/openid-configuration",
		func(w http.ResponseWriter, _ *http.Request) {
			_ = json.NewEncoder(w).Encode(elephantine.OpenIDConnectConfig{
				Issuer:  srv.URL,
				JwksURI: srv.URL + "/idp/jwks.json",
			})
		})

	mux.HandleFunc("/idp/jwks.json",
		func(w http.ResponseWriter, _ *http.Request) {
			_, _ = w.Write([]byte(`{"keys":[]}`))
		})

	mux.HandleFunc("/delegate/jwks.json",
		func(w http.ResponseWriter, _ *http.Request) {
			_ = json.NewEncoder(w).Encode(map[string]any{
				"keys": []map[string]string{{
					"kty": "OKP",
					"crv": "Ed25519",
					"x":   base64.RawURLEncoding.EncodeToString(pub),
					"kid": "delegate-1",
					"use": "sig",
					"alg": "EdDSA",
				}},
			})
		})

	settings := elephantine.AuthenticationSettings{
		OIDCConfig:     srv.URL + "/.well-known/openid-configuration",
		DelegateIssuer: testDelegateIssuer,
		DelegateJWKS:   srv.URL + "/delegate/jwks.json",
	}

	_, err = elephantine.AuthenticationConfigFromSettings(
		t.Context(), settings, nil)
	test.MustNotf(t, err, "refuse a delegate issuer without an audience")

	noJWKS := settings
	noJWKS.Audience = testDelegateAudience
	noJWKS.DelegateJWKS = ""

	_, err = elephantine.AuthenticationConfigFromSettings(
		t.Context(), noJWKS, nil)
	test.MustNotf(t, err, "refuse a delegate issuer without a JWKS")

	settings.Audience = testDelegateAudience

	conf, err := elephantine.AuthenticationConfigFromSettings(
		t.Context(), settings, nil)
	test.Mustf(t, err, "configure the delegate issuer")

	token := jwt.NewWithClaims(jwt.SigningMethodEdDSA,
		delegateClaims(testDelegateAudience))
	token.Header["kid"] = "delegate-1"

	ss, err := token.SignedString(key)
	test.Mustf(t, err, "sign the delegate token")

	_, err = conf.AuthParser.AuthInfoFromHeader("Bearer " + ss)
	test.Mustf(t, err, "accept a delegate token verified against the JWKS")
}
