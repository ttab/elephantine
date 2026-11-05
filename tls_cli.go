package elephantine

import "github.com/urfave/cli/v3"

// DefaultTLSAddr is where the HTTPS listener runs unless tls-addr says
// otherwise, the port helm-elib's tlsSupport exposes by default.
const DefaultTLSAddr = ":1443"

// TLSCLIFlags returns the flags APIServerTLSFromCLI reads: tls-addr
// (TLS_ADDR or TLS_LISTEN_ADDR, DefaultTLSAddr by default), cert-file
// (TLS_CERT_PATH) and key-file (TLS_KEY_PATH). The environment variables are
// the contract with helm-elib, which mounts the cert-manager certificate and
// sets them when tlsSupport is on, so a service using these flags gets its
// HTTPS listener from that setting alone.
func TLSCLIFlags() []cli.Flag {
	return []cli.Flag{
		&cli.StringFlag{
			Name: "tls-addr",
			Usage: "Address of the HTTPS listener, which serves " +
				"HTTP/1.1 and HTTP/2 (negotiated through ALPN). " +
				"Only started when cert-file and key-file are set",
			Value:   DefaultTLSAddr,
			Sources: cli.EnvVars("TLS_ADDR", "TLS_LISTEN_ADDR"),
		},
		&cli.StringFlag{
			Name:    "cert-file",
			Usage:   "TLS certificate (PEM); enables the HTTPS listener",
			Sources: cli.EnvVars("TLS_CERT_PATH"),
		},
		&cli.StringFlag{
			Name:    "key-file",
			Usage:   "TLS private key (PEM) for cert-file",
			Sources: cli.EnvVars("TLS_KEY_PATH"),
		},
	}
}

// APIServerTLSFromCLI is APIServerTLS with the values of TLSCLIFlags. The
// HTTPS listener serves HTTP/1.1 and HTTP/2 negotiated through ALPN, which is
// what an ingress target group speaking HTTP/2 to its pods reaches, and
// reloads the certificate when the files change. Without a certificate only
// the plaintext listener runs.
func APIServerTLSFromCLI(cmd *cli.Command) APIServerOption {
	return APIServerTLS(
		cmd.String("tls-addr"), cmd.String("cert-file"), cmd.String("key-file"))
}
