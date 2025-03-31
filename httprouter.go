package elephantine

import (
	"errors"
	"io"
	"maps"
	"net/http"

	"github.com/julienschmidt/httprouter"
)

// RHandleFunc creates a httprouter.Handle from a function that can return an
// error. If the error is a HTTPError the information it carries will be used
// for the error response. Otherwise it will be treated as a internal server
// eror and the error message will be sent as the response.
//
// Deprecated: use the standard library muxer and HTTPErrorHandlerFunc instead.
func RHandleFunc(
	fn func(http.ResponseWriter, *http.Request, httprouter.Params) error,
) httprouter.Handle {
	return func(w http.ResponseWriter, r *http.Request, p httprouter.Params) {
		err := fn(w, r, p)
		if err != nil {
			writeHTTPError(w, err)
		}
	}
}

// HTTPErrorHandlerFunc creates a http.HandlerFunc from a function that can
// return an error. If the error is a HTTPError the information it carries will
// be used for the error response. Otherwise it will be treated as a internal
// server error and the error message will be sent as the response.
func HTTPErrorHandlerFunc(
	fn func(http.ResponseWriter, *http.Request) error,
) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		err := fn(w, r)
		if err != nil {
			writeHTTPError(w, err)
		}
	}
}

func writeHTTPError(w http.ResponseWriter, err error) {
	var httpErr *HTTPError

	if !errors.As(err, &httpErr) {
		http.Error(w, err.Error(), http.StatusInternalServerError)

		return
	}

	if httpErr.Header != nil {
		maps.Copy(w.Header(), httpErr.Header)
	}

	statusCode := httpErr.StatusCode
	if statusCode == 0 {
		statusCode = http.StatusInternalServerError
	}

	w.WriteHeader(statusCode)

	_, _ = io.Copy(w, httpErr.Body)
}
