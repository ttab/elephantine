package elephantine

import (
	"fmt"
	"net/http"
	"net/url"
	"strings"

	"github.com/ryanuber/go-glob"
)

// CORSOptions configures the CORS middleware and the AllowOrigin check: which
// origins are allowed, which methods and headers are permitted, and how long
// preflight responses may be cached.
type CORSOptions struct {
	AllowInsecure          bool
	AllowInsecureLocalhost bool
	Hosts                  []string
	HostPatterns           []string
	AllowedMethods         []string
	AllowedHeaders         []string
	MaxAgeSeconds          int

	// PublicPrefixes are request path prefixes that are open to any origin.
	// A request whose path starts with one of them is answered with
	// "Access-Control-Allow-Origin: *" and no "Vary: Origin", and a
	// preflight for it is allowed whatever the Origin header says. Requests
	// to any other path are unaffected and go through the Hosts and
	// HostPatterns checks as before.
	//
	// This is for anonymous read surfaces that sit behind a CDN: a response
	// that varies on Origin is cached once per embedding site, and an
	// origin allowlist is meaningless for content anyone may fetch without
	// credentials anyway. Never mark a path that reads the caller's
	// authorization or cookies as public — the browser will not send
	// credentials to a wildcard origin, but a shared cache in front of the
	// service would still be free to serve one caller's response to
	// another.
	//
	// Prefixes are matched literally with strings.HasPrefix, so include the
	// trailing slash ("/public/") unless you mean to cover every path that
	// merely starts with the string.
	PublicPrefixes []string
}

func CORSMiddleware(opts CORSOptions, handler http.Handler) http.Handler {
	if opts.MaxAgeSeconds == 0 {
		opts.MaxAgeSeconds = 3600
	}

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		accessMethod := r.Header.Get("Access-Control-Request-Method")
		origin := r.Header.Get("Origin")
		header := w.Header()
		public := opts.publicPath(r.URL.Path)

		if r.Method == http.MethodOptions && accessMethod != "" {
			if !public && !opts.AllowOrigin(origin) {
				w.WriteHeader(http.StatusMethodNotAllowed)

				return
			}

			allowOrigin := origin
			if public {
				allowOrigin = "*"
			}

			header.Set("Access-Control-Allow-Methods",
				strings.Join(opts.AllowedMethods, ","))
			header.Set("Access-Control-Allow-Headers",
				strings.Join(opts.AllowedHeaders, ","))
			header.Set("Access-Control-Allow-Origin",
				allowOrigin)
			header.Set("Access-Control-Max-Age",
				fmt.Sprintf("%d", opts.MaxAgeSeconds))

			w.WriteHeader(http.StatusNoContent)

			return
		}

		switch {
		case public:
			// No Vary: the response is the same for every origin,
			// and a Vary would give a CDN one cache entry per
			// embedding site.
			header.Set("Access-Control-Allow-Origin", "*")
		case origin != "" && opts.AllowOrigin(origin):
			header.Set("Access-Control-Allow-Origin", origin)
			header.Set("Vary", "Origin")
		}

		handler.ServeHTTP(w, r)
	})
}

// publicPath reports whether the request path is covered by one of the
// PublicPrefixes.
func (opts CORSOptions) publicPath(path string) bool {
	for _, prefix := range opts.PublicPrefixes {
		if strings.HasPrefix(path, prefix) {
			return true
		}
	}

	return false
}

// AllowOrigin reports whether the given Origin header value is
// accepted under these options. Exposed so that non-CORS code paths
// (notably WebSocket upgrades) can validate Origin with the same
// rules as the CORS middleware:
//
//   - Origin is parsed and only its hostname is considered (port
//     stripped).
//   - The scheme must be https unless AllowInsecure is set, or the
//     hostname is "localhost" and AllowInsecureLocalhost is set.
//   - Hosts entries match the hostname exactly or as a parent
//     domain (entry "tt.se" matches "tt.se" and "foo.tt.se").
//   - HostPatterns entries are go-glob patterns matched against the
//     hostname.
func (opts CORSOptions) AllowOrigin(origin string) bool {
	oURL, err := url.Parse(origin)
	if err != nil {
		return false
	}

	allowInsec := opts.AllowInsecure ||
		(oURL.Hostname() == "localhost" && opts.AllowInsecureLocalhost)

	if !allowInsec && oURL.Scheme != "https" {
		return false
	}

	host := oURL.Hostname()

	for _, h := range opts.Hosts {
		if host == h || strings.HasSuffix(host, "."+h) {
			return true
		}
	}

	for _, h := range opts.HostPatterns {
		if glob.Glob(h, host) {
			return true
		}
	}

	return false
}
