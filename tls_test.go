package elephantine_test

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"fmt"
	"log/slog"
	"math/big"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/ttab/elephantine"
	"github.com/ttab/elephantine/test"
)

func generateSelfSignedCert(
	t *testing.T, cn string, ipAddresses ...net.IP,
) (certPEM []byte, keyPEM []byte) {
	t.Helper()

	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	test.Mustf(t, err, "generate ECDSA key")

	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	test.Mustf(t, err, "generate serial number")

	template := x509.Certificate{
		SerialNumber: serial,
		Subject: pkix.Name{
			CommonName: cn,
		},
		IPAddresses: ipAddresses,
		NotBefore:   time.Now().Add(-time.Hour),
		NotAfter:    time.Now().Add(24 * time.Hour),
		KeyUsage:    x509.KeyUsageDigitalSignature,
		ExtKeyUsage: []x509.ExtKeyUsage{
			x509.ExtKeyUsageServerAuth,
		},
	}

	certDER, err := x509.CreateCertificate(
		rand.Reader, &template, &template, &key.PublicKey, key,
	)
	test.Mustf(t, err, "create certificate")

	keyDER, err := x509.MarshalECPrivateKey(key)
	test.Mustf(t, err, "marshal EC private key")

	certPEM = pem.EncodeToMemory(&pem.Block{
		Type:  "CERTIFICATE",
		Bytes: certDER,
	})

	keyPEM = pem.EncodeToMemory(&pem.Block{
		Type:  "EC PRIVATE KEY",
		Bytes: keyDER,
	})

	return certPEM, keyPEM
}

func writeCertFiles(
	t *testing.T, dir string, certPEM []byte, keyPEM []byte,
) (string, string) {
	t.Helper()

	certFile := filepath.Join(dir, "cert.pem")
	keyFile := filepath.Join(dir, "key.pem")

	err := os.WriteFile(certFile, certPEM, 0o600)
	test.Mustf(t, err, "write cert file")

	err = os.WriteFile(keyFile, keyPEM, 0o600)
	test.Mustf(t, err, "write key file")

	return certFile, keyFile
}

func TestCertificateSource(t *testing.T) {
	logger := slog.New(test.NewLogHandler(t, slog.LevelDebug))
	dir := t.TempDir()

	certPEM1, keyPEM1 := generateSelfSignedCert(t, "initial.example.com")
	certFile, keyFile := writeCertFiles(t, dir, certPEM1, keyPEM1)

	cs, err := elephantine.NewCertificateSource(
		logger, certFile, keyFile,
		elephantine.CertSourcePollInterval(50*time.Millisecond),
		elephantine.CertSourceSettleDelay(200*time.Millisecond),
	)
	test.Mustf(t, err, "create certificate source")

	cert1, err := cs.GetCertificate(nil)
	test.Mustf(t, err, "get initial certificate")

	if cert1 == nil {
		t.Fatal("initial certificate should not be nil")
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	done := make(chan struct{})

	go func() {
		defer close(done)

		runErr := cs.Run(ctx)
		if runErr != nil {
			t.Errorf("unexpected Run error: %v", runErr)
		}
	}()

	// Wait a bit for the watcher to start, then write a new cert.
	time.Sleep(100 * time.Millisecond)

	certPEM2, keyPEM2 := generateSelfSignedCert(t, "rotated.example.com")

	err = os.WriteFile(certFile, certPEM2, 0o600)
	test.Mustf(t, err, "overwrite cert file")

	err = os.WriteFile(keyFile, keyPEM2, 0o600)
	test.Mustf(t, err, "overwrite key file")

	// Wait for the rotated certificate rather than for a fixed margin over
	// the poll interval and settle delay, which a machine under load
	// outlasts.
	cert2 := waitForCertCN(t, cs, "rotated.example.com", 10*time.Second)

	if cert2 == cert1 {
		t.Fatal("certificate should have been reloaded")
	}

	cancel()
	<-done
}

// certSampleInterval is how often a test samples the served certificate, and
// certSampleSlack is the tolerance that follows from sampling: a write is
// timed once the file has been written, and a reload is seen up to a sample
// interval after it happened.
const (
	certSampleInterval = 5 * time.Millisecond
	certSampleSlack    = 10 * time.Millisecond
)

// certReloadWatcher records when the certificate a source serves changes,
// which is the only externally visible sign of a reload.
type certReloadWatcher struct {
	mu      sync.Mutex
	reloads []time.Time
}

// Reloads returns the times at which the served certificate changed.
func (w *certReloadWatcher) Reloads() []time.Time {
	w.mu.Lock()
	defer w.mu.Unlock()

	return slices.Clone(w.reloads)
}

// WaitFor waits for at least n reloads to have been recorded and returns them.
// A test that spotted a reload by looking at the certificate itself still has
// to wait for the watcher to sample it, which is why this takes a deadline
// rather than reading the slice.
func (w *certReloadWatcher) WaitFor(
	t *testing.T, n int, within time.Duration,
) []time.Time {
	t.Helper()

	deadline := time.Now().Add(within)

	for {
		reloads := w.Reloads()
		if len(reloads) >= n {
			return reloads
		}

		if !time.Now().Before(deadline) {
			t.Fatalf("expected at least %d reloads within %s, saw %d",
				n, within, len(reloads))
		}

		time.Sleep(certSampleInterval)
	}
}

// watchCertReloads samples the served certificate until the context is
// cancelled, recording when it changes. Sampling is what lets a test say when
// a reload happened, rather than whether one had happened by the time it got
// round to looking, which is a race against the settle delay.
func watchCertReloads(
	ctx context.Context, t *testing.T, cs *elephantine.CertificateSource,
) *certReloadWatcher {
	t.Helper()

	previous, err := cs.GetCertificate(nil)
	test.Mustf(t, err, "get initial certificate")

	var w certReloadWatcher

	done := make(chan struct{})

	go func() {
		defer close(done)

		ticker := time.NewTicker(certSampleInterval)
		defer ticker.Stop()

		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
			}

			current, err := cs.GetCertificate(nil)
			if err != nil {
				t.Errorf("sample certificate: %v", err)

				return
			}

			if current == previous {
				continue
			}

			seen := time.Now()

			w.mu.Lock()
			w.reloads = append(w.reloads, seen)
			w.mu.Unlock()

			previous = current
		}
	}()

	// The watcher reports through t, so it has to be finished before the
	// test is.
	t.Cleanup(func() {
		<-done
	})

	return &w
}

// waitForCertCN waits for the source to serve a certificate with the given
// common name, and returns it. Waiting for the certificate the test is after
// costs nothing when the machine is quick, and does not fail when it isn't —
// unlike sleeping for a margin over the poll interval and settle delay.
func waitForCertCN(
	t *testing.T, cs *elephantine.CertificateSource, cn string,
	within time.Duration,
) *tls.Certificate {
	t.Helper()

	deadline := time.Now().Add(within)

	var served string

	for {
		current, err := cs.GetCertificate(nil)
		test.Mustf(t, err, "get certificate")

		parsed, err := x509.ParseCertificate(current.Certificate[0])
		test.Mustf(t, err, "parse certificate")

		if parsed.Subject.CommonName == cn {
			return current
		}

		served = parsed.Subject.CommonName

		if !time.Now().Before(deadline) {
			t.Fatalf("the certificate for %q was not loaded within %s, serving %q",
				cn, within, served)
		}

		time.Sleep(certSampleInterval)
	}
}

// lastWriteBefore returns the latest write at or before the given time.
func lastWriteBefore(writes []time.Time, at time.Time) (time.Time, bool) {
	var (
		latest time.Time
		found  bool
	)

	for _, w := range writes {
		if w.After(at) {
			continue
		}

		if !found || w.After(latest) {
			latest = w
			found = true
		}
	}

	return latest, found
}

// TestCertificateSourceSettleDebounce verifies that a burst of writes is
// reloaded after it ends rather than once per write.
//
// The debounce is a statement about the gap between a write and the reload
// that follows it, so that is what the test asserts, against the times it
// recorded. Asserting instead that no reload had happened by the end of the
// write loop made the test a race: the reload is legitimately due a settle
// delay later, so any stall between the last write and the check — a busy CI
// runner is enough — failed a source that had behaved perfectly. Timing the
// gaps also keeps the property honest on a machine that did stall, since a
// stall spreads the writes out and a reload between two of them is then
// correct.
func TestCertificateSourceSettleDebounce(t *testing.T) {
	const (
		pollInterval = 50 * time.Millisecond
		settleDelay  = 300 * time.Millisecond
		writeGap     = 100 * time.Millisecond
		writeCount   = 5
		patience     = 10 * time.Second
	)

	logger := slog.New(test.NewLogHandler(t, slog.LevelDebug))
	dir := t.TempDir()

	certPEM, keyPEM := generateSelfSignedCert(t, "debounce-initial.example.com")
	certFile, keyFile := writeCertFiles(t, dir, certPEM, keyPEM)

	cs, err := elephantine.NewCertificateSource(
		logger, certFile, keyFile,
		elephantine.CertSourcePollInterval(pollInterval),
		elephantine.CertSourceSettleDelay(settleDelay),
	)
	test.Mustf(t, err, "create certificate source")

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	watcher := watchCertReloads(ctx, t, cs)

	done := make(chan struct{})

	go func() {
		defer close(done)

		_ = cs.Run(ctx)
	}()

	// Overwrite the files repeatedly, faster than the settle delay, and
	// record when each write landed.
	var (
		writeTimes []time.Time
		lastCN     string
	)

	for i := range writeCount {
		time.Sleep(writeGap)

		lastCN = fmt.Sprintf("debounce-%d.example.com", i)

		newCert, newKey := generateSelfSignedCert(t, lastCN)

		err = os.WriteFile(certFile, newCert, 0o600)
		test.Mustf(t, err, "overwrite cert file")

		err = os.WriteFile(keyFile, newKey, 0o600)
		test.Mustf(t, err, "overwrite key file")

		writeTimes = append(writeTimes, time.Now())
	}

	// The pair that ends up loaded is the last one written, not an earlier
	// one the source raced its way to.
	waitForCertCN(t, cs, lastCN, patience)

	// No reload may land closer than a settle delay behind the write
	// before it. A source that reloaded on every write would land one poll
	// interval behind each of them.
	for i, at := range watcher.WaitFor(t, 1, patience) {
		write, ok := lastWriteBefore(writeTimes, at)
		if !ok {
			t.Fatalf("reload %d happened before any write", i+1)
		}

		gap := at.Sub(write)

		if gap < settleDelay-certSampleSlack {
			t.Fatalf("reload %d came %s after the write before it, inside the %s settle delay",
				i+1, gap, settleDelay)
		}
	}

	cancel()
	<-done
}

func TestCertificateSourceBadReload(t *testing.T) {
	logger := slog.New(test.NewLogHandler(t, slog.LevelDebug))
	dir := t.TempDir()

	certPEM, keyPEM := generateSelfSignedCert(t, "bad-reload.example.com")
	certFile, keyFile := writeCertFiles(t, dir, certPEM, keyPEM)

	cs, err := elephantine.NewCertificateSource(
		logger, certFile, keyFile,
		elephantine.CertSourcePollInterval(50*time.Millisecond),
		elephantine.CertSourceSettleDelay(200*time.Millisecond),
	)
	test.Mustf(t, err, "create certificate source")

	goodCert, err := cs.GetCertificate(nil)
	test.Mustf(t, err, "get initial certificate")

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	done := make(chan struct{})

	go func() {
		defer close(done)

		_ = cs.Run(ctx)
	}()

	time.Sleep(100 * time.Millisecond)

	// Write invalid content to both files.
	err = os.WriteFile(certFile, []byte("not a cert"), 0o600)
	test.Mustf(t, err, "write invalid cert")

	err = os.WriteFile(keyFile, []byte("not a key"), 0o600)
	test.Mustf(t, err, "write invalid key")

	// Wait for poll + settle + margin.
	time.Sleep(500 * time.Millisecond)

	// The old cert should still be served.
	currentCert, err := cs.GetCertificate(nil)
	test.Mustf(t, err, "get certificate after bad reload")

	if currentCert != goodCert {
		t.Fatal("certificate should not have changed after failed reload")
	}

	cancel()
	<-done
}

func TestCertificateSourceHTTPServer(t *testing.T) {
	logger := slog.New(test.NewLogHandler(t, slog.LevelDebug))
	dir := t.TempDir()
	loopback := net.IPv4(127, 0, 0, 1)

	certPEM1, keyPEM1 := generateSelfSignedCert(
		t, "initial.example.com", loopback,
	)
	certFile, keyFile := writeCertFiles(t, dir, certPEM1, keyPEM1)

	cs, err := elephantine.NewCertificateSource(
		logger, certFile, keyFile,
		elephantine.CertSourcePollInterval(50*time.Millisecond),
		elephantine.CertSourceSettleDelay(200*time.Millisecond),
	)
	test.Mustf(t, err, "create certificate source")

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	watchDone := make(chan struct{})

	go func() {
		defer close(watchDone)

		_ = cs.Run(ctx)
	}()

	// Start a TLS server on a random port.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	test.Mustf(t, err, "listen on random port")

	server := http.Server{
		Handler: http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			_, _ = fmt.Fprintln(w, "ok")
		}),
		TLSConfig: &tls.Config{
			GetCertificate: cs.GetCertificate,
		},
		ReadHeaderTimeout: 5 * time.Second,
	}

	serverDone := make(chan struct{})

	go func() {
		defer close(serverDone)

		sErr := server.ServeTLS(ln, "", "")
		if sErr != nil && !errors.Is(sErr, http.ErrServerClosed) {
			t.Errorf("unexpected server error: %v", sErr)
		}
	}()

	t.Cleanup(func() {
		_ = server.Close()

		<-serverDone
	})

	addr := ln.Addr().String()

	// Build a pool with the first cert so we can verify it.
	pool1 := x509.NewCertPool()
	pool1.AppendCertsFromPEM(certPEM1)

	peerCN := tlsConnectAndGetCN(t, addr, pool1)
	if peerCN != "initial.example.com" {
		t.Fatalf("expected CN 'initial.example.com', got %q", peerCN)
	}

	// Rotate the certificate.
	certPEM2, keyPEM2 := generateSelfSignedCert(
		t, "rotated.example.com", loopback,
	)

	err = os.WriteFile(certFile, certPEM2, 0o600)
	test.Mustf(t, err, "overwrite cert file")

	err = os.WriteFile(keyFile, keyPEM2, 0o600)
	test.Mustf(t, err, "overwrite key file")

	// Wait for poll + settle + margin.
	time.Sleep(500 * time.Millisecond)

	// Build a pool with the second cert.
	pool2 := x509.NewCertPool()
	pool2.AppendCertsFromPEM(certPEM2)

	peerCN = tlsConnectAndGetCN(t, addr, pool2)
	if peerCN != "rotated.example.com" {
		t.Fatalf("expected CN 'rotated.example.com', got %q", peerCN)
	}

	cancel()
	<-watchDone
}

func tlsConnectAndGetCN(
	t *testing.T, addr string, rootCAs *x509.CertPool,
) string {
	t.Helper()

	conn, err := tls.Dial("tcp", addr, &tls.Config{
		RootCAs: rootCAs,
	})
	test.Mustf(t, err, "TLS dial")

	defer func() {
		_ = conn.Close()
	}()

	state := conn.ConnectionState()

	if len(state.PeerCertificates) == 0 {
		t.Fatal("no peer certificates")
	}

	return state.PeerCertificates[0].Subject.CommonName
}
