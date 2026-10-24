package rpc_test

import (
	"crypto/rand"
	"crypto/rsa"
	"strings"
	"testing"

	"connectrpc.com/connect"
	"github.com/golang-jwt/jwt/v5"
	"github.com/ttab/elephantine/internal/testservice"
	"github.com/ttab/elephantine/rpc"
	"github.com/ttab/elephantine/test"
)

// TestDPoPProofAttacks sends proofs that are wrong in the ways an attacker, or
// a sloppy client, would get them wrong, and checks that each is refused for
// the reason it is about, on both stacks. The proofs that RFC 9449 says must be
// accepted, differing from the request only in what the comparison ignores,
// are checked to be accepted.
func TestDPoPProofAttacks(t *testing.T) {
	var (
		d     = newDelegate(t)
		s     = newDelegateStack(t, d, &rpc.DPoPOptions{PublicURL: publicURL})
		token = d.Sign(t, d.Claims(t))
	)

	rsaKey, err := rsa.GenerateKey(rand.Reader, 2048)
	test.Mustf(t, err, "create an RSA key")

	htu := func(change func(string) string) proof {
		return proof{Tamper: func(_ map[string]any, c jwt.MapClaims) {
			u, _ := c["htu"].(string)
			c["htu"] = change(u)
		}}
	}

	header := func(change func(h map[string]any)) proof {
		return proof{Tamper: func(h map[string]any, _ jwt.MapClaims) {
			change(h)
		}}
	}

	jwk := func(change func(k map[string]any)) proof {
		return header(func(h map[string]any) {
			k, _ := h["jwk"].(map[string]any)
			change(k)
		})
	}

	claim := func(name string, value any) proof {
		return proof{Tamper: func(_ map[string]any, c jwt.MapClaims) {
			if value == nil {
				delete(c, name)

				return
			}

			c[name] = value
		}}
	}

	refused := map[string]struct {
		proof  proof
		reason string
	}{
		"duplicate_header": {proof{Duplicate: true}, "more than one DPoP header"},
		"trailing_slash": {htu(func(u string) string {
			return u + "/"
		}), reasonWrongHTU},
		"http_scheme": {htu(func(u string) string {
			return strings.Replace(u, "https://", "http://", 1)
		}), reasonWrongHTU},
		"explicit_port": {htu(func(u string) string {
			return strings.Replace(u, ".com/", ".com:8443/", 1)
		}), reasonWrongHTU},
		"other_path_prefix": {htu(func(u string) string {
			return strings.Replace(u, ".com/", ".com/x/", 1)
		}), reasonWrongHTU},
		"relative_htu": {htu(func(u string) string {
			return strings.TrimPrefix(u, publicURL)
		}), reasonWrongHTU},
		"lowercase_htm": {claim("htm", "post"), "wrong htm"},
		"no_ath":        {claim("ath", nil), "wrong ath"},
		"no_jti":        {claim("jti", nil), "no jti"},
		"empty_jti":     {claim("jti", ""), "no jti"},
		"no_iat":        {claim("iat", nil), "no iat"},
		"hs256": {proof{
			Alg: jwt.SigningMethodHS256, SignKey: []byte("secret"),
		}, "signing method HS256 is invalid"},
		"none": {proof{
			Alg:     jwt.SigningMethodNone,
			SignKey: jwt.UnsafeAllowNoneSignatureType,
		}, "invalid DPoP proof"},
		"rs256": {proof{
			Alg: jwt.SigningMethodRS256, SignKey: rsaKey,
		}, "signing method RS256 is invalid"},
		"wrong_typ": {header(func(h map[string]any) {
			h["typ"] = "JWT"
		}), "wrong typ"},
		"no_typ": {header(func(h map[string]any) {
			delete(h, "typ")
		}), "wrong typ"},
		"no_jwk": {header(func(h map[string]any) {
			delete(h, "jwk")
		}), "no jwk"},
		"private_jwk": {jwk(func(k map[string]any) {
			k["d"] = "AAAA"
		}), "private key"},
		"other_curve": {jwk(func(k map[string]any) {
			k["crv"] = "P-384"
		}), "unsupported curve"},
		"symmetric_jwk": {jwk(func(k map[string]any) {
			k["kty"] = "oct"
		}), "unsupported key type"},
		"point_off_curve": {jwk(func(k map[string]any) {
			k["y"] = k["x"]
		}), "invalid public key"},
	}

	for name, c := range refused {
		t.Run(name, func(t *testing.T) {
			for proto, client := range boundClients(t, s, d, token, c.proof) {
				t.Run(proto, func(t *testing.T) {
					_, err := client.Echo(t.Context(),
						&testservice.EchoRequest{Message: echoMessage})

					test.IsRPCError(t, err, connect.CodeUnauthenticated)

					if !strings.Contains(err.Error(), c.reason) {
						t.Fatalf("expected %q in the error, got %v",
							c.reason, err)
					}
				})
			}
		})
	}

	accepted := map[string]proof{
		// RFC 9449 section 4.3: the htu is compared without its query
		// and fragment.
		"query_and_fragment": htu(func(u string) string {
			return u + "?x=1#y"
		}),
		// Scheme and host are case-insensitive.
		"host_case": htu(func(u string) string {
			return strings.Replace(u, "https://stream.example.com",
				"HTTPS://Stream.Example.COM", 1)
		}),
		// The proof is what binds the token, whichever scheme it is
		// sent with.
		"bearer_scheme": {Scheme: "Bearer"},
	}

	for name, p := range accepted {
		t.Run(name, func(t *testing.T) {
			for proto, client := range boundClients(t, s, d, token, p) {
				t.Run(proto, func(t *testing.T) {
					_, err := client.Echo(t.Context(),
						&testservice.EchoRequest{Message: echoMessage})
					test.Mustf(t, err, "accept the proof")
				})
			}
		})
	}
}
