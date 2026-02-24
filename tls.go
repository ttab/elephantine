package elephantine

import (
	"context"
	"crypto/tls"
	"fmt"
	"log/slog"
	"os"
	"sync"
	"time"
)

// CertificateSource manages a TLS certificate that is automatically reloaded
// when the underlying files change on disk. It polls the certificate and key
// files for modification time changes and reloads after a settle delay to avoid
// loading partially written files.
type CertificateSource struct {
	certFile     string
	keyFile      string
	logger       *slog.Logger
	pollInterval time.Duration
	settleDelay  time.Duration

	mu          sync.RWMutex
	cert        *tls.Certificate
	certModTime time.Time
	keyModTime  time.Time
}

// CertificateSourceOption configures a CertificateSource.
type CertificateSourceOption func(*CertificateSource)

// CertSourcePollInterval overrides the default poll interval (5s).
func CertSourcePollInterval(d time.Duration) CertificateSourceOption {
	return func(cs *CertificateSource) {
		cs.pollInterval = d
	}
}

// CertSourceSettleDelay overrides the default settle delay (10s).
func CertSourceSettleDelay(d time.Duration) CertificateSourceOption {
	return func(cs *CertificateSource) {
		cs.settleDelay = d
	}
}

// NewCertificateSource creates a CertificateSource that loads the initial
// certificate and key pair from the given files. It returns an error if the
// initial load fails.
func NewCertificateSource(
	logger *slog.Logger, certFile string, keyFile string,
	opts ...CertificateSourceOption,
) (*CertificateSource, error) {
	cs := CertificateSource{
		certFile:     certFile,
		keyFile:      keyFile,
		logger:       logger,
		pollInterval: 5 * time.Second,
		settleDelay:  10 * time.Second,
	}

	for _, opt := range opts {
		opt(&cs)
	}

	err := cs.loadCertificate()
	if err != nil {
		return nil, fmt.Errorf("load initial certificate: %w", err)
	}

	return &cs, nil
}

// GetCertificate returns the current TLS certificate. It is intended to be used
// as the tls.Config.GetCertificate callback.
func (cs *CertificateSource) GetCertificate(
	_ *tls.ClientHelloInfo,
) (*tls.Certificate, error) {
	cs.mu.RLock()
	defer cs.mu.RUnlock()

	return cs.cert, nil
}

// Run polls the certificate and key files for changes and reloads the
// certificate after the settle delay. It returns nil when the context is
// cancelled.
func (cs *CertificateSource) Run(ctx context.Context) error {
	ticker := time.NewTicker(cs.pollInterval)
	defer ticker.Stop()

	var settleTimer *time.Timer
	var settleCh <-chan time.Time

	// Track the last observed mod times separately from the loaded mod
	// times. This avoids resetting the settle timer on every poll tick when
	// the files have been changed but not yet reloaded.
	var seenCertMod, seenKeyMod time.Time

	for {
		select {
		case <-ctx.Done():
			if settleTimer != nil {
				settleTimer.Stop()
			}

			return nil
		case <-ticker.C:
			certMod, keyMod, err := cs.fileModTimes()
			if err != nil {
				cs.logger.Error("check certificate files",
					"err", err)

				continue
			}

			if !cs.filesChanged(certMod, keyMod) {
				continue
			}

			// Only reset the settle timer if the files have
			// changed since we last observed them (i.e. more writes
			// are happening).
			if certMod.Equal(seenCertMod) && keyMod.Equal(seenKeyMod) {
				continue
			}

			seenCertMod = certMod
			seenKeyMod = keyMod

			if settleTimer == nil {
				cs.logger.Info(
					"certificate file change detected, waiting for settle",
					"delay", cs.settleDelay,
				)

				settleTimer = time.NewTimer(cs.settleDelay)
				settleCh = settleTimer.C
			} else {
				settleTimer.Reset(cs.settleDelay)
			}
		case <-settleCh:
			settleTimer = nil
			settleCh = nil

			err := cs.loadCertificate()
			if err != nil {
				cs.logger.Error("reload certificate",
					"err", err)

				continue
			}

			cs.logger.Info("reloaded TLS certificate")
		}
	}
}

func (cs *CertificateSource) fileModTimes() (time.Time, time.Time, error) {
	certInfo, err := os.Stat(cs.certFile)
	if err != nil {
		return time.Time{}, time.Time{},
			fmt.Errorf("stat certificate file: %w", err)
	}

	keyInfo, err := os.Stat(cs.keyFile)
	if err != nil {
		return time.Time{}, time.Time{},
			fmt.Errorf("stat key file: %w", err)
	}

	return certInfo.ModTime(), keyInfo.ModTime(), nil
}

func (cs *CertificateSource) filesChanged(
	certMod time.Time, keyMod time.Time,
) bool {
	cs.mu.RLock()
	loadedCertMod := cs.certModTime
	loadedKeyMod := cs.keyModTime
	cs.mu.RUnlock()

	return !certMod.Equal(loadedCertMod) || !keyMod.Equal(loadedKeyMod)
}

func (cs *CertificateSource) loadCertificate() error {
	cert, err := tls.LoadX509KeyPair(cs.certFile, cs.keyFile)
	if err != nil {
		return fmt.Errorf("load X509 key pair: %w", err)
	}

	certInfo, err := os.Stat(cs.certFile)
	if err != nil {
		return fmt.Errorf("stat certificate file: %w", err)
	}

	keyInfo, err := os.Stat(cs.keyFile)
	if err != nil {
		return fmt.Errorf("stat key file: %w", err)
	}

	cs.mu.Lock()
	cs.cert = &cert
	cs.certModTime = certInfo.ModTime()
	cs.keyModTime = keyInfo.ModTime()
	cs.mu.Unlock()

	return nil
}
