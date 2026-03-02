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
	"fmt"
	"log/slog"
	"math/big"
	"net"
	"net/http"
	"os"
	"path/filepath"
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
	test.Must(t, err, "generate ECDSA key")

	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	test.Must(t, err, "generate serial number")

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
	test.Must(t, err, "create certificate")

	keyDER, err := x509.MarshalECPrivateKey(key)
	test.Must(t, err, "marshal EC private key")

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
	test.Must(t, err, "write cert file")

	err = os.WriteFile(keyFile, keyPEM, 0o600)
	test.Must(t, err, "write key file")

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
	test.Must(t, err, "create certificate source")

	cert1, err := cs.GetCertificate(nil)
	test.Must(t, err, "get initial certificate")

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
	test.Must(t, err, "overwrite cert file")

	err = os.WriteFile(keyFile, keyPEM2, 0o600)
	test.Must(t, err, "overwrite key file")

	// Wait for poll + settle + some margin.
	time.Sleep(500 * time.Millisecond)

	cert2, err := cs.GetCertificate(nil)
	test.Must(t, err, "get rotated certificate")

	if cert2 == cert1 {
		t.Fatal("certificate should have been reloaded")
	}

	parsed, err := x509.ParseCertificate(cert2.Certificate[0])
	test.Must(t, err, "parse rotated certificate")

	if parsed.Subject.CommonName != "rotated.example.com" {
		t.Fatalf("expected CN 'rotated.example.com', got %q",
			parsed.Subject.CommonName)
	}

	cancel()
	<-done
}

func TestCertificateSourceSettleDebounce(t *testing.T) {
	logger := slog.New(test.NewLogHandler(t, slog.LevelDebug))
	dir := t.TempDir()

	certPEM, keyPEM := generateSelfSignedCert(t, "debounce.example.com")
	certFile, keyFile := writeCertFiles(t, dir, certPEM, keyPEM)

	cs, err := elephantine.NewCertificateSource(
		logger, certFile, keyFile,
		elephantine.CertSourcePollInterval(50*time.Millisecond),
		elephantine.CertSourceSettleDelay(300*time.Millisecond),
	)
	test.Must(t, err, "create certificate source")

	initialCert, err := cs.GetCertificate(nil)
	test.Must(t, err, "get initial certificate")

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	done := make(chan struct{})

	go func() {
		defer close(done)

		_ = cs.Run(ctx)
	}()

	// Rapidly overwrite files multiple times to trigger repeated settle
	// resets.
	for range 5 {
		time.Sleep(100 * time.Millisecond)

		newCert, newKey := generateSelfSignedCert(t, "debounce.example.com")

		err = os.WriteFile(certFile, newCert, 0o600)
		test.Must(t, err, "overwrite cert file")

		err = os.WriteFile(keyFile, newKey, 0o600)
		test.Must(t, err, "overwrite key file")
	}

	// During the rapid writes the cert should not have been reloaded yet.
	midCert, err := cs.GetCertificate(nil)
	test.Must(t, err, "get mid-write certificate")

	if midCert != initialCert {
		t.Fatal("certificate should not have been reloaded during rapid writes")
	}

	// Wait for settle.
	time.Sleep(500 * time.Millisecond)

	reloadedCert, err := cs.GetCertificate(nil)
	test.Must(t, err, "get reloaded certificate")

	if reloadedCert == initialCert {
		t.Fatal("certificate should have been reloaded after settle")
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
	test.Must(t, err, "create certificate source")

	goodCert, err := cs.GetCertificate(nil)
	test.Must(t, err, "get initial certificate")

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
	test.Must(t, err, "write invalid cert")

	err = os.WriteFile(keyFile, []byte("not a key"), 0o600)
	test.Must(t, err, "write invalid key")

	// Wait for poll + settle + margin.
	time.Sleep(500 * time.Millisecond)

	// The old cert should still be served.
	currentCert, err := cs.GetCertificate(nil)
	test.Must(t, err, "get certificate after bad reload")

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
	test.Must(t, err, "create certificate source")

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	watchDone := make(chan struct{})

	go func() {
		defer close(watchDone)

		_ = cs.Run(ctx)
	}()

	// Start a TLS server on a random port.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	test.Must(t, err, "listen on random port")

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
		if sErr != nil && sErr != http.ErrServerClosed {
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
	test.Must(t, err, "overwrite cert file")

	err = os.WriteFile(keyFile, keyPEM2, 0o600)
	test.Must(t, err, "overwrite key file")

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
	test.Must(t, err, "TLS dial")

	defer func() {
		_ = conn.Close()
	}()

	state := conn.ConnectionState()

	if len(state.PeerCertificates) == 0 {
		t.Fatal("no peer certificates")
	}

	return state.PeerCertificates[0].Subject.CommonName
}
