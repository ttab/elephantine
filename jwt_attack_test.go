package elephantine_test

import (
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"encoding/base64"
	"encoding/json"
	"errors"
	"testing"

	"github.com/MicahParks/keyfunc/v3"
	"github.com/golang-jwt/jwt/v5"
	"github.com/ttab/elephantine"
	"github.com/ttab/elephantine/test"
)

// attackParser is a parser that trusts an identity provider and the
// delegate, both through JWKS key functions, with the same kid in both sets so
// that a kid collision across the issuers is part of every attack.
type attackParser struct {
	parser      *elephantine.JWTAuthInfoParser
	idpKey      *ecdsa.PrivateKey
	delegateKey ed25519.PrivateKey
}

const collidingKID = "k1"

func newAttackParser(t *testing.T) attackParser {
	t.Helper()

	idpKey, err := ecdsa.GenerateKey(elliptic.P384(), rand.Reader)
	test.Mustf(t, err, "create the identity provider key")

	pub, delegateKey, err := ed25519.GenerateKey(rand.Reader)
	test.Mustf(t, err, "create the delegate key")

	idpRaw, err := idpKey.PublicKey.Bytes()
	test.Mustf(t, err, "encode the identity provider key")

	// The JWK members are fields rather than map keys, which keeps the
	// member names in one place.
	type jwk struct {
		Kty string `json:"kty"`
		Crv string `json:"crv"`
		X   string `json:"x"`
		Y   string `json:"y,omitempty"`
		Kid string `json:"kid"`
	}

	jwks := func(key jwk) jwt.Keyfunc {
		raw, err := json.Marshal(map[string]any{
			"keys": []jwk{key},
		})
		test.Mustf(t, err, "encode the JWKS")

		k, err := keyfunc.NewJWKSetJSON(raw)
		test.Mustf(t, err, "create the key function")

		return k.Keyfunc
	}

	idpKeys := jwks(jwk{
		Kty: "EC",
		Crv: "P-384",
		X:   base64.RawURLEncoding.EncodeToString(idpRaw[1:49]),
		Y:   base64.RawURLEncoding.EncodeToString(idpRaw[49:]),
		Kid: collidingKID,
	})

	delegateKeys := jwks(jwk{
		Kty: "OKP",
		Crv: "Ed25519",
		X:   base64.RawURLEncoding.EncodeToString(pub),
		Kid: collidingKID,
	})

	parser := elephantine.NewJWTAuthInfoParser(t.Context(), idpKeys,
		elephantine.JWTAuthInfoParserOptions{
			Issuer:   testIssuer,
			Audience: testDelegateAudience,
			TrustedIssuers: []elephantine.TrustedIssuer{{
				Issuer:  testDelegateIssuer,
				Keyfunc: delegateKeys,
			}},
		})

	return attackParser{
		parser:      parser,
		idpKey:      idpKey,
		delegateKey: delegateKey,
	}
}

func signWith(
	t *testing.T, method jwt.SigningMethod, key any, claims jwt.Claims,
) string {
	t.Helper()

	token := jwt.NewWithClaims(method, claims)
	token.Header["kid"] = collidingKID

	ss, err := token.SignedString(key)
	test.Mustf(t, err, "sign the token")

	return ss
}

// TestTokenAlgorithmConfusion checks that a token is only accepted when signed
// with an asymmetric algorithm, by the issuer it names, with that issuer's
// kind of key, also when the issuers' key sets share a kid.
func TestTokenAlgorithmConfusion(t *testing.T) {
	p := newAttackParser(t)

	unbound := func(iss string) elephantine.JWTClaims {
		c := delegateClaims(testDelegateAudience)
		c.Issuer = iss
		c.Confirmation = nil

		return c
	}

	_, err := p.parser.AuthInfoFromToken(signWith(t,
		jwt.SigningMethodEdDSA, p.delegateKey, unbound(testDelegateIssuer)))
	test.Mustf(t, err, "accept the delegate's own token")

	_, err = p.parser.AuthInfoFromToken(signWith(t,
		jwt.SigningMethodES384, p.idpKey, unbound(testIssuer)))
	test.Mustf(t, err, "accept the identity provider's own token")

	attackerRSA, err := rsa.GenerateKey(rand.Reader, 2048)
	test.Mustf(t, err, "create an attacker RSA key")

	delegatePub, _ := p.delegateKey.Public().(ed25519.PublicKey)

	idpPubRaw, err := p.idpKey.PublicKey.Bytes()
	test.Mustf(t, err, "encode the identity provider key")

	attacks := map[string]string{
		"none_as_delegate": signWith(t, jwt.SigningMethodNone,
			jwt.UnsafeAllowNoneSignatureType, unbound(testDelegateIssuer)),
		"none_as_idp": signWith(t, jwt.SigningMethodNone,
			jwt.UnsafeAllowNoneSignatureType, unbound(testIssuer)),
		"hs256_with_delegate_public_key": signWith(t,
			jwt.SigningMethodHS256, []byte(delegatePub),
			unbound(testDelegateIssuer)),
		"hs256_with_idp_public_key": signWith(t,
			jwt.SigningMethodHS256, idpPubRaw, unbound(testIssuer)),
		"delegate_key_claiming_idp": signWith(t,
			jwt.SigningMethodEdDSA, p.delegateKey, unbound(testIssuer)),
		"idp_key_claiming_delegate": signWith(t,
			jwt.SigningMethodES384, p.idpKey, unbound(testDelegateIssuer)),
		"rs256_as_delegate": signWith(t, jwt.SigningMethodRS256,
			attackerRSA, unbound(testDelegateIssuer)),
		"rs256_as_idp": signWith(t, jwt.SigningMethodRS256,
			attackerRSA, unbound(testIssuer)),
	}

	for name, token := range attacks {
		t.Run(name, func(t *testing.T) {
			_, err := p.parser.AuthInfoFromToken(token)
			test.MustNotf(t, err, "refuse the token")
		})
	}
}

// TestBoundTokenOutsideMiddleware checks that a token bound to a key is not
// handed to code that takes the parser's result as authentication, such as a
// websocket handler, since nothing there verifies the DPoP proof.
func TestBoundTokenOutsideMiddleware(t *testing.T) {
	p := newAttackParser(t)

	bound := signWith(t, jwt.SigningMethodEdDSA, p.delegateKey,
		delegateClaims(testDelegateAudience))

	_, err := p.parser.AuthInfoFromHeader("Bearer " + bound)
	test.MustNotf(t, err, "refuse a bound token in a Bearer header")

	if !errors.Is(err, elephantine.ErrBoundToken) {
		t.Fatalf("expected ErrBoundToken, got %v", err)
	}

	_, err = p.parser.AuthInfoFromHeader("DPoP " + bound)
	test.MustNotf(t, err, "refuse a bound token in a DPoP header")

	_, err = p.parser.AuthInfoFromToken(bound)
	test.MustNotf(t, err, "refuse a bound token")

	var claims elephantine.JWTClaims

	_, err = p.parser.ValidateTokenWithClaims(bound, &claims)
	test.MustNotf(t, err, "refuse to validate a bound token")

	var mapClaims jwt.MapClaims

	_, err = p.parser.ValidateTokenWithClaims(bound, &mapClaims)
	test.MustNotf(t, err,
		"refuse to validate a bound token into other claims")

	// The middleware's way in, which holds the info back until the
	// proof is verified.
	for _, scheme := range []string{"Bearer ", "DPoP "} {
		info, err := p.parser.PendingAuthInfoFromHeader(scheme + bound)
		test.Mustf(t, err, "accept a bound token pending its proof")

		_, ok := elephantine.GetAuthInfo(
			elephantine.SetAuthInfo(t.Context(), info))
		test.Equalf(t, false, ok,
			"hide the bound token until its proof is verified")
	}
}

// TestScopeSeparators checks that scopes are separated by spaces only, as in
// RFC 6749. Splitting on other whitespace would let a single scope that an
// issuer checked as one, such as "doc_read:x<tab>doc_admin", be read here as
// two.
func TestScopeSeparators(t *testing.T) {
	for _, sep := range []string{"\t", "\n", " ", " ", "\v"} {
		claims := elephantine.JWTClaims{
			Scope: "doc_read:x" + sep + "doc_admin",
		}

		test.Equalf(t, false, claims.HasScope("doc_admin"),
			"%q does not separate scopes", sep)
		test.Equalf(t, false, claims.HasAnyScope("doc_admin"),
			"%q does not separate scopes for any", sep)

		stripped := elephantine.StripScopePrefix(
			"test_doc_read:x"+sep+"test_doc_admin", testScopePrefix)

		claims.Scope = stripped

		test.Equalf(t, false, claims.HasScope("doc_admin"),
			"stripping the prefix does not turn %q into a separator", sep)
	}

	claims := elephantine.JWTClaims{Scope: " doc_read  doc_write "}

	test.Equalf(t, true, claims.HasScope("doc_write"),
		"tolerate repeated and surrounding spaces")
	test.Equalf(t, false, claims.HasScope(""),
		"never match the empty scope")
}
