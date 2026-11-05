package elephantine_test

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"syscall"
	"testing"
	"time"

	"github.com/ttab/elephantine"
	"github.com/ttab/elephantine/test"
	"github.com/urfave/cli/v3"
)

// TestAPIServerTLSFromCLI: with the certificate variables helm-elib sets,
// the HTTPS listener negotiates HTTP/2 through ALPN and still serves a
// client that offers HTTP/1.1 only.
func TestAPIServerTLSFromCLI(t *testing.T) {
	certPEM, keyPEM := generateSelfSignedCert(
		t, "tls-cli.example.com", net.IPv4(127, 0, 0, 1))
	certFile, keyFile := writeCertFiles(t, t.TempDir(), certPEM, keyPEM)

	roots := x509.NewCertPool()
	roots.AppendCertsFromPEM(certPEM)

	tlsAddr := freeLoopbackAddr(t)

	t.Setenv("TLS_ADDR", tlsAddr)
	t.Setenv("TLS_CERT_PATH", certFile)
	t.Setenv("TLS_KEY_PATH", keyFile)

	server := protoServerFromCLI(t, freeLoopbackAddr(t))

	runAPIServer(t, server)

	for _, tc := range []struct {
		name string
		h2   bool
		want string
	}{
		{name: "h2", h2: true, want: "HTTP/2.0"},
		{name: "http/1.1", h2: false, want: "HTTP/1.1"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			transport := http.Transport{
				TLSClientConfig: &tls.Config{
					RootCAs:    roots,
					MinVersion: tls.VersionTLS12,
				},
				Protocols: new(http.Protocols),
			}

			transport.Protocols.SetHTTP1(true)
			transport.Protocols.SetHTTP2(tc.h2)

			got := pollBody(t, &http.Client{Transport: &transport},
				"https://"+tlsAddr+"/proto")
			if got != tc.want {
				t.Errorf("served over %q, want %q", got, tc.want)
			}
		})
	}
}

// TestAPIServerTLSFromCLIWithoutCertificate: without the certificate
// variables only the plaintext listener runs.
func TestAPIServerTLSFromCLIWithoutCertificate(t *testing.T) {
	addr := freeLoopbackAddr(t)
	tlsAddr := freeLoopbackAddr(t)

	t.Setenv("TLS_ADDR", tlsAddr)
	t.Setenv("TLS_CERT_PATH", "")
	t.Setenv("TLS_KEY_PATH", "")

	server := protoServerFromCLI(t, addr)

	runAPIServer(t, server)

	pollBody(t, http.DefaultClient, "http://"+addr+"/proto")

	conn, err := net.DialTimeout("tcp", tlsAddr, time.Second)
	if err == nil {
		_ = conn.Close()

		t.Fatalf("something listens on %s with no certificate configured",
			tlsAddr)
	}

	if !errors.Is(err, syscall.ECONNREFUSED) {
		t.Fatalf("dial %s: %v, want connection refused", tlsAddr, err)
	}
}

// protoServerFromCLI parses TLSCLIFlags from the environment and builds an
// API server on addr with APIServerTLSFromCLI, with a handler that answers
// with the protocol it was served over. The health listener is off.
func protoServerFromCLI(t *testing.T, addr string) *elephantine.APIServer {
	t.Helper()

	logger := slog.New(test.NewLogHandler(t, slog.LevelInfo))

	var server *elephantine.APIServer

	cmd := cli.Command{
		Name:  "run",
		Flags: elephantine.TLSCLIFlags(),
		Action: func(_ context.Context, c *cli.Command) error {
			server = elephantine.NewAPIServer(logger, addr, "",
				elephantine.APIServerTLSFromCLI(c))

			return nil
		},
	}

	test.Mustf(t, cmd.Run(t.Context(), []string{"run"}), "parse flags")

	server.Mux.HandleFunc("GET /proto", func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, r.Proto)
	})

	return server
}

// runAPIServer runs the server until the test ends, and fails the test if
// it stops with an error or does not stop.
func runAPIServer(t *testing.T, server *elephantine.APIServer) {
	t.Helper()

	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)

	go func() {
		done <- server.ListenAndServe(ctx)
	}()

	t.Cleanup(func() {
		cancel()

		select {
		case err := <-done:
			if err != nil {
				t.Errorf("server stopped with an error: %v", err)
			}
		case <-time.After(15 * time.Second):
			t.Error("server did not stop within 15s")
		}
	})
}

// pollBody polls url until it answers 200 and returns the body.
func pollBody(t *testing.T, client *http.Client, url string) string {
	t.Helper()

	end := time.Now().Add(10 * time.Second)

	for time.Now().Before(end) {
		resp, err := client.Get(url) //nolint:noctx // test poll.
		if err != nil {
			time.Sleep(20 * time.Millisecond)

			continue
		}

		body, err := io.ReadAll(resp.Body)
		_ = resp.Body.Close()

		if err == nil && resp.StatusCode == http.StatusOK {
			return string(body)
		}

		time.Sleep(20 * time.Millisecond)
	}

	t.Fatalf("%s did not answer within 10s", url)

	return ""
}

// freeLoopbackAddr returns a loopback address with a port that was free a
// moment ago. The API server takes addresses rather than listeners, so the
// window between closing this one and the server binding it is
// unavoidable.
func freeLoopbackAddr(t *testing.T) string {
	t.Helper()

	l, err := net.Listen("tcp", "127.0.0.1:0")
	test.Mustf(t, err, "find a free port")

	addr := l.Addr().String()

	test.Mustf(t, l.Close(), "release the port")

	return addr
}
