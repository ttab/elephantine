package elephantine

import (
	"fmt"
	"net/http"
	"net/url"
	"strings"

	"github.com/ryanuber/go-glob"
)

type CORSOptions struct {
	AllowInsecure          bool
	AllowInsecureLocalhost bool
	Hosts                  []string
	HostPatterns           []string
	AllowedMethods         []string
	AllowedHeaders         []string
	MaxAgeSeconds          int
}

func CORSMiddleware(opts CORSOptions, handler http.Handler) http.Handler {
	if opts.MaxAgeSeconds == 0 {
		opts.MaxAgeSeconds = 3600
	}

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		accessMethod := r.Header.Get("Access-Control-Request-Method")
		origin := r.Header.Get("Origin")
		header := w.Header()

		if r.Method == http.MethodOptions && accessMethod != "" {

			if !opts.AllowOrigin(origin) {
				w.WriteHeader(http.StatusMethodNotAllowed)

				return
			}

			header.Set("Access-Control-Allow-Methods",
				strings.Join(opts.AllowedMethods, ","))
			header.Set("Access-Control-Allow-Headers",
				strings.Join(opts.AllowedHeaders, ","))
			header.Set("Access-Control-Allow-Origin",
				origin)
			header.Set("Access-Control-Max-Age",
				fmt.Sprintf("%d", opts.MaxAgeSeconds))

			w.WriteHeader(http.StatusNoContent)

			return
		}

		if origin != "" && opts.AllowOrigin(origin) {
			header.Set("Access-Control-Allow-Origin", origin)
			header.Set("Vary", "Origin")
		}

		handler.ServeHTTP(w, r)
	})
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
