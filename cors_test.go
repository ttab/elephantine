package elephantine_test

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/ttab/elephantine"
	"github.com/ttab/elephantine/test"
)

const (
	originTT      = "https://tt.se"
	originUnknown = "https://example.com"
	hostLocalhost = "localhost"
	hostTT        = "tt.se"
	headerAuth    = "Authorization"
	headerCT      = "Content-Type"
	pathPublic    = "/public/blog/123"
	pathPrivate   = "/twirp/service/Method"
)

type corsTestCase struct {
	Origin       string
	ExpectStatus int
}

var cases = map[string]corsTestCase{
	"valid_origin": {
		Origin:       originTT,
		ExpectStatus: http.StatusNoContent,
	},
	"valid_subdomain_origin": {
		Origin:       "https://www.tt.se",
		ExpectStatus: http.StatusNoContent,
	},
	"valid_local_origin": {
		Origin:       "https://localhost",
		ExpectStatus: http.StatusNoContent,
	},
	"valid_insecure_local_origin": {
		Origin:       "http://localhost",
		ExpectStatus: http.StatusNoContent,
	},
	"valid_insecure_local_origin_port": {
		Origin:       "http://localhost:5173",
		ExpectStatus: http.StatusNoContent,
	},
	"invalid_origin": {
		Origin:       originUnknown,
		ExpectStatus: http.StatusMethodNotAllowed,
	},
	"sneaky_invalid_origin": {
		Origin:       "https://examplett.se",
		ExpectStatus: http.StatusMethodNotAllowed,
	},
	"insecure_origin": {
		Origin:       "http://tt.se",
		ExpectStatus: http.StatusMethodNotAllowed,
	},
}

func TestCORSMiddleware(t *testing.T) {
	yesMan := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})

	withCors := elephantine.CORSMiddleware(elephantine.CORSOptions{
		AllowInsecure:          false,
		AllowInsecureLocalhost: true,
		Hosts:                  []string{hostLocalhost, hostTT},
		AllowedMethods:         []string{http.MethodGet},
		AllowedHeaders:         []string{headerAuth, headerCT},
	}, yesMan)

	server := httptest.NewServer(withCors)

	client := server.Client()

	for name := range cases {
		tc := cases[name]

		t.Run(name, func(t *testing.T) {
			req, err := http.NewRequest(http.MethodOptions, server.URL, nil)
			test.Mustf(t, err, "create test request")

			req.Header.Set("Access-Control-Request-Method", http.MethodGet)
			req.Header.Set("Access-Control-Request-Headers", headerAuth)
			req.Header.Set("Origin", tc.Origin)

			res, err := client.Do(req)
			test.Mustf(t, err, "make request")

			test.Mustf(t, res.Body.Close(), "close response body")

			test.Equalf(t, tc.ExpectStatus, res.StatusCode,
				"get correct status code")
		})
	}
}

func TestCORSMiddlewarePatterns(t *testing.T) {
	yesMan := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})

	withCors := elephantine.CORSMiddleware(elephantine.CORSOptions{
		AllowInsecure:          false,
		AllowInsecureLocalhost: true,
		HostPatterns:           []string{hostLocalhost, hostTT, "*.tt.se"},
		AllowedMethods:         []string{http.MethodGet},
		AllowedHeaders:         []string{headerAuth, headerCT},
	}, yesMan)

	server := httptest.NewServer(withCors)

	client := server.Client()

	for name := range cases {
		tc := cases[name]

		t.Run(name, func(t *testing.T) {
			req, err := http.NewRequest(http.MethodOptions, server.URL, nil)
			test.Mustf(t, err, "create test request")

			req.Header.Set("Access-Control-Request-Method", http.MethodGet)
			req.Header.Set("Access-Control-Request-Headers", headerAuth)
			req.Header.Set("Origin", tc.Origin)

			res, err := client.Do(req)
			test.Mustf(t, err, "make request")

			test.Mustf(t, res.Body.Close(), "close response body")

			test.Equalf(t, tc.ExpectStatus, res.StatusCode,
				"get correct status code")
		})
	}
}

type publicCORSTestCase struct {
	Path         string
	Method       string
	Origin       string
	ExpectStatus int
	ExpectOrigin string
	ExpectVary   string
}

// TestCORSMiddlewarePublicPrefixes covers the PublicPrefixes behaviour: paths
// under a public prefix answer any origin with a wildcard and no Vary, and
// everything else keeps the allowlist behaviour unchanged.
func TestCORSMiddlewarePublicPrefixes(t *testing.T) {
	publicCases := map[string]publicCORSTestCase{
		"public_get_unknown_origin": {
			Path:         pathPublic,
			Method:       http.MethodGet,
			Origin:       originUnknown,
			ExpectStatus: http.StatusOK,
			ExpectOrigin: "*",
		},
		"public_get_allowed_origin": {
			Path:         pathPublic,
			Method:       http.MethodGet,
			Origin:       originTT,
			ExpectStatus: http.StatusOK,
			ExpectOrigin: "*",
		},
		"public_get_no_origin": {
			Path:         pathPublic,
			Method:       http.MethodGet,
			ExpectStatus: http.StatusOK,
			ExpectOrigin: "*",
		},
		"public_get_insecure_origin": {
			Path:         pathPublic,
			Method:       http.MethodGet,
			Origin:       "http://example.com",
			ExpectStatus: http.StatusOK,
			ExpectOrigin: "*",
		},
		"prefix_is_literal": {
			Path:         "/publicity",
			Method:       http.MethodGet,
			Origin:       originUnknown,
			ExpectStatus: http.StatusOK,
			ExpectOrigin: "",
		},
		"private_get_unknown_origin": {
			Path:         pathPrivate,
			Method:       http.MethodGet,
			Origin:       originUnknown,
			ExpectStatus: http.StatusOK,
			ExpectOrigin: "",
		},
		"private_get_allowed_origin": {
			Path:         pathPrivate,
			Method:       http.MethodGet,
			Origin:       originTT,
			ExpectStatus: http.StatusOK,
			ExpectOrigin: originTT,
			ExpectVary:   "Origin",
		},
	}

	yesMan := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})

	withCors := elephantine.CORSMiddleware(elephantine.CORSOptions{
		AllowInsecure:          false,
		AllowInsecureLocalhost: true,
		Hosts:                  []string{hostLocalhost, hostTT},
		AllowedMethods:         []string{http.MethodGet},
		AllowedHeaders:         []string{headerAuth, headerCT},
		PublicPrefixes:         []string{"/public/"},
	}, yesMan)

	server := httptest.NewServer(withCors)

	client := server.Client()

	for name := range publicCases {
		tc := publicCases[name]

		t.Run(name, func(t *testing.T) {
			req, err := http.NewRequestWithContext(
				t.Context(), tc.Method, server.URL+tc.Path, nil)
			test.Mustf(t, err, "create test request")

			if tc.Origin != "" {
				req.Header.Set("Origin", tc.Origin)
			}

			res, err := client.Do(req)
			test.Mustf(t, err, "make request")

			test.Mustf(t, res.Body.Close(), "close response body")

			test.Equalf(t, tc.ExpectStatus, res.StatusCode,
				"get correct status code")
			test.Equalf(t, tc.ExpectOrigin,
				res.Header.Get("Access-Control-Allow-Origin"),
				"get correct allowed origin")
			test.Equalf(t, tc.ExpectVary, res.Header.Get("Vary"),
				"get correct vary header")
		})
	}
}

// TestCORSMiddlewarePublicPreflight verifies that a preflight for a public
// path is answered with a 204 and a wildcard origin whatever the Origin header
// says, while a preflight for a private path still rejects unknown origins.
func TestCORSMiddlewarePublicPreflight(t *testing.T) {
	preflightCases := map[string]publicCORSTestCase{
		"public_unknown_origin": {
			Path:         pathPublic,
			Origin:       originUnknown,
			ExpectStatus: http.StatusNoContent,
			ExpectOrigin: "*",
		},
		"public_insecure_origin": {
			Path:         pathPublic,
			Origin:       "http://example.com",
			ExpectStatus: http.StatusNoContent,
			ExpectOrigin: "*",
		},
		"public_no_origin": {
			Path:         pathPublic,
			ExpectStatus: http.StatusNoContent,
			ExpectOrigin: "*",
		},
		"public_allowed_origin": {
			Path:         pathPublic,
			Origin:       originTT,
			ExpectStatus: http.StatusNoContent,
			ExpectOrigin: "*",
		},
		"private_unknown_origin": {
			Path:         pathPrivate,
			Origin:       originUnknown,
			ExpectStatus: http.StatusMethodNotAllowed,
			ExpectOrigin: "",
		},
		"private_allowed_origin": {
			Path:         pathPrivate,
			Origin:       originTT,
			ExpectStatus: http.StatusNoContent,
			ExpectOrigin: originTT,
		},
	}

	yesMan := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})

	withCors := elephantine.CORSMiddleware(elephantine.CORSOptions{
		AllowInsecure:          false,
		AllowInsecureLocalhost: true,
		Hosts:                  []string{hostLocalhost, hostTT},
		AllowedMethods:         []string{http.MethodGet},
		AllowedHeaders:         []string{headerAuth, headerCT},
		PublicPrefixes:         []string{"/public/"},
	}, yesMan)

	server := httptest.NewServer(withCors)

	client := server.Client()

	for name := range preflightCases {
		tc := preflightCases[name]

		t.Run(name, func(t *testing.T) {
			req, err := http.NewRequestWithContext(t.Context(),
				http.MethodOptions, server.URL+tc.Path, nil)
			test.Mustf(t, err, "create test request")

			req.Header.Set("Access-Control-Request-Method",
				http.MethodGet)

			if tc.Origin != "" {
				req.Header.Set("Origin", tc.Origin)
			}

			res, err := client.Do(req)
			test.Mustf(t, err, "make request")

			test.Mustf(t, res.Body.Close(), "close response body")

			test.Equalf(t, tc.ExpectStatus, res.StatusCode,
				"get correct status code")
			test.Equalf(t, tc.ExpectOrigin,
				res.Header.Get("Access-Control-Allow-Origin"),
				"get correct allowed origin")
			test.Equalf(t, "", res.Header.Get("Vary"),
				"get no vary header")
		})
	}
}
