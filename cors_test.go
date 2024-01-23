package elephantine_test

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/ttab/elephantine"
	"github.com/ttab/elephantine/test"
)

type corsTestCase struct {
	Origin       string
	ExpectStatus int
}

func TestCORSMiddleware(t *testing.T) {
	yesMan := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})

	withCors := elephantine.CORSMiddleware(elephantine.CORSOptions{
		AllowInsecure:          false,
		AllowInsecureLocalhost: true,
		Hosts:                  []string{"localhost", "tt.se"},
		AllowedMethods:         []string{"GET"},
		AllowedHeaders:         []string{"Authorization", "Content-Type"},
	}, yesMan)

	server := httptest.NewServer(withCors)

	client := server.Client()

	cases := map[string]corsTestCase{
		"valid_origin": {
			Origin:       "https://tt.se",
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
		"invalid_origin": {
			Origin:       "https://example.com",
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

	for name := range cases {
		tc := cases[name]

		t.Run(name, func(t *testing.T) {
			req, err := http.NewRequest(http.MethodOptions, server.URL, nil)
			test.Must(t, err, "create test request")

			req.Header.Set("Access-Control-Request-Method", http.MethodGet)
			req.Header.Set("Access-Control-Request-Headers", "Authorization")
			req.Header.Set("Origin", tc.Origin)

			res, err := client.Do(req)
			test.Must(t, err, "make request")

			test.Must(t, res.Body.Close(), "close response body")

			test.Equal(t, tc.ExpectStatus, res.StatusCode,
				"get correct status code")
		})
	}
}
