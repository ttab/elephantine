package elephantine_test

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/ttab/elephantine"
	"github.com/ttab/elephantine/test"
)

// drainHandler reads the whole request body and reports how it went: 200 with
// the byte count, or 413 if the body ran past the limit installed by the
// middleware.
func drainHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n, err := io.Copy(io.Discard, r.Body)

		var maxErr *http.MaxBytesError

		switch {
		case errors.As(err, &maxErr):
			w.WriteHeader(http.StatusRequestEntityTooLarge)
		case err != nil:
			w.WriteHeader(http.StatusInternalServerError)
		default:
			_, _ = fmt.Fprintf(w, "%d", n)
		}
	})
}

func TestMaxBodyBytesMiddleware(t *testing.T) {
	const limit = 1024

	handler := elephantine.MaxBodyBytesMiddleware(limit, drainHandler())

	t.Run("under_the_limit", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodPost, "/",
			strings.NewReader(strings.Repeat("a", limit)))
		rec := httptest.NewRecorder()

		handler.ServeHTTP(rec, req)

		test.Equalf(t, http.StatusOK, rec.Code, "get correct status code")
		test.Equalf(t, fmt.Sprintf("%d", limit), rec.Body.String(),
			"read the whole body")
	})

	t.Run("declared_over_the_limit", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodPost, "/",
			strings.NewReader("a"))
		req.ContentLength = limit + 1
		rec := httptest.NewRecorder()

		handler.ServeHTTP(rec, req)

		test.Equalf(t, http.StatusRequestEntityTooLarge, rec.Code,
			"refuse an over-declared body without reading it")
	})

	t.Run("streamed_over_the_limit", func(t *testing.T) {
		// A body of unknown length only fails once the handler reads
		// past the limit.
		req := httptest.NewRequest(http.MethodPost, "/",
			io.NopCloser(bytes.NewReader(
				bytes.Repeat([]byte("a"), limit+1))))
		req.ContentLength = -1
		rec := httptest.NewRecorder()

		handler.ServeHTTP(rec, req)

		test.Equalf(t, http.StatusRequestEntityTooLarge, rec.Code,
			"fail the read that passes the limit")
	})

	t.Run("no_limit", func(t *testing.T) {
		unlimited := elephantine.MaxBodyBytesMiddleware(0, drainHandler())

		req := httptest.NewRequest(http.MethodPost, "/",
			strings.NewReader("a"))
		req.ContentLength = 1 << 40
		rec := httptest.NewRecorder()

		unlimited.ServeHTTP(rec, req)

		test.Equalf(t, http.StatusOK, rec.Code,
			"let any body through when the limit is disabled")
	})
}

func TestAPIServerMaxBodyBytes(t *testing.T) {
	const limit = 1024

	logger := slog.New(test.NewLogHandler(t, slog.LevelDebug))

	srv, client := elephantine.NewTestAPIServer(t, logger,
		elephantine.APIServerMaxBodyBytes(limit),
	)

	srv.Mux.Handle("POST /drain", drainHandler())

	err := srv.ListenAndServe(context.Background())
	test.Mustf(t, err, "start test API server")

	cases := map[string]struct {
		Size         int
		ExpectStatus int
	}{
		"at_the_limit": {
			Size:         limit,
			ExpectStatus: http.StatusOK,
		},
		"over_the_limit": {
			Size:         limit + 1,
			ExpectStatus: http.StatusRequestEntityTooLarge,
		},
	}

	for name := range cases {
		tc := cases[name]

		t.Run(name, func(t *testing.T) {
			req, err := http.NewRequestWithContext(t.Context(),
				http.MethodPost, "http://"+srv.Addr()+"/drain",
				strings.NewReader(strings.Repeat("a", tc.Size)))
			test.Mustf(t, err, "create test request")

			res, err := client.Do(req)
			test.Mustf(t, err, "make request")

			test.Mustf(t, res.Body.Close(), "close response body")

			test.Equalf(t, tc.ExpectStatus, res.StatusCode,
				"get correct status code")
		})
	}
}

// TestAPIServerDefaultMaxBodyBytes checks that an API server that wasn't given
// APIServerMaxBodyBytes still bounds request bodies at DefaultMaxBodyBytes.
//
// The request is written by hand so that only the headers are sent: an
// over-declared Content-Length is refused before the body is read, so the test
// doesn't have to push eight mebibytes through the loopback to see the 413.
func TestAPIServerDefaultMaxBodyBytes(t *testing.T) {
	logger := slog.New(test.NewLogHandler(t, slog.LevelDebug))

	srv, _ := elephantine.NewTestAPIServer(t, logger)

	srv.Mux.Handle("POST /drain", drainHandler())

	err := srv.ListenAndServe(context.Background())
	test.Mustf(t, err, "start test API server")

	conn, err := net.Dial("tcp", srv.Addr())
	test.Mustf(t, err, "connect to the test API server")

	defer func() {
		_ = conn.Close()
	}()

	err = conn.SetDeadline(time.Now().Add(10 * time.Second))
	test.Mustf(t, err, "set connection deadline")

	_, err = fmt.Fprintf(conn,
		"POST /drain HTTP/1.1\r\nHost: %s\r\nContent-Length: %d\r\n\r\n",
		srv.Addr(), elephantine.DefaultMaxBodyBytes+1)
	test.Mustf(t, err, "write request headers")

	res, err := http.ReadResponse(bufio.NewReader(conn), nil)
	test.Mustf(t, err, "read response")

	test.Mustf(t, res.Body.Close(), "close response body")

	test.Equalf(t, http.StatusRequestEntityTooLarge, res.StatusCode,
		"refuse a body over the default limit")
}

func TestAPIServerPublicCORS(t *testing.T) {
	logger := slog.New(test.NewLogHandler(t, slog.LevelDebug))

	srv, client := elephantine.NewTestAPIServer(t, logger,
		elephantine.APIServerCORSHosts("tt.se"),
		elephantine.APIServerPublicCORS("/public/"),
	)

	srv.Mux.Handle("GET /public/thing", http.HandlerFunc(func(
		w http.ResponseWriter, _ *http.Request,
	) {
		w.WriteHeader(http.StatusOK)
	}))

	srv.Mux.Handle("GET /private/thing", http.HandlerFunc(func(
		w http.ResponseWriter, _ *http.Request,
	) {
		w.WriteHeader(http.StatusOK)
	}))

	err := srv.ListenAndServe(context.Background())
	test.Mustf(t, err, "start test API server")

	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet,
		"http://"+srv.Addr()+"/public/thing", nil)
	test.Mustf(t, err, "create test request")

	req.Header.Set("Origin", "https://example.com")

	res, err := client.Do(req)
	test.Mustf(t, err, "make request")

	test.Mustf(t, res.Body.Close(), "close response body")

	test.Equalf(t, "*", res.Header.Get("Access-Control-Allow-Origin"),
		"allow any origin under the public prefix")
	test.Equalf(t, "", res.Header.Get("Vary"),
		"not vary on origin under the public prefix")

	privReq, err := http.NewRequestWithContext(t.Context(), http.MethodGet,
		"http://"+srv.Addr()+"/private/thing", nil)
	test.Mustf(t, err, "create test request")

	privReq.Header.Set("Origin", "https://example.com")

	privRes, err := client.Do(privReq)
	test.Mustf(t, err, "make request")

	test.Mustf(t, privRes.Body.Close(), "close response body")

	test.Equalf(t, "", privRes.Header.Get("Access-Control-Allow-Origin"),
		"keep the origin allowlist outside the public prefix")
}
