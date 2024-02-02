package elephantine

import (
	"fmt"
	"net/http"
	"net/url"
	"strings"
)

type CORSOptions struct {
	AllowInsecure          bool
	AllowInsecureLocalhost bool
	Hosts                  []string
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

		if r.Method == http.MethodOptions && accessMethod != "" {
			origin := r.Header.Get("Origin")

			if !validOrigin(origin, opts) {
				w.WriteHeader(http.StatusMethodNotAllowed)

				return
			}

			header := w.Header()

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

		handler.ServeHTTP(w, r)
	})
}

func validOrigin(origin string, opts CORSOptions) bool {
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

	return false
}
