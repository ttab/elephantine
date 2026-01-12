package elephantine

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"

	jsoniter "github.com/json-iterator/go"
	"github.com/ttab/ttninjs"
)

type TTNINJSErrorCause string

const (
	TTNINJSErrorCauseNotFound    TTNINJSErrorCause = "not_found"
	TTNINJSErrorCauseInvalidURI  TTNINJSErrorCause = "invalid_uri"
	TTNINJSErrorCauseInvalidDoc  TTNINJSErrorCause = "invalid_doc"
	TTNINJSErrorCauseInvalidBody TTNINJSErrorCause = "invalid_body"
)

type TTNINJSPermanentError struct {
	Cause TTNINJSErrorCause
}

func (err TTNINJSPermanentError) Error() string {
	return "permanent error: " + string(err.Cause)
}

type Media struct {
	logger *slog.Logger
	client *http.Client
	host   string
}

func NewMedia(
	logger *slog.Logger,
	client *http.Client,
	host string,
) *Media {
	return &Media{
		logger: logger,
		client: client,
		host:   host,
	}
}

func (m *Media) GetRenderedTTNINJS(
	ctx context.Context, docURI string, _ []byte,
) (_ ttninjs.Document, outError error) {
	parsedURI, err := url.Parse(docURI)
	if err != nil {
		return ttninjs.Document{}, fmt.Errorf("invalid document URI: %w",
			errors.Join(err, TTNINJSPermanentError{
				Cause: TTNINJSErrorCauseInvalidURI,
			}))
	}

	// Switch to the correct host for the environment and add a JSON suffix
	// to get the JSON representation of the document.
	parsedURI.Host = m.host
	parsedURI.Scheme = "https"
	parsedURI.Path += ".json"

	req, err := http.NewRequest(http.MethodGet, parsedURI.String(), nil)
	if err != nil {
		return ttninjs.Document{}, fmt.Errorf(
			"create request: %w",
			errors.Join(err, TTNINJSPermanentError{
				Cause: TTNINJSErrorCauseInvalidURI,
			}))
	}

	res, err := m.client.Do(req.WithContext(ctx)) //nolint: bodyclose
	if err != nil {
		return ttninjs.Document{}, fmt.Errorf(
			"perform request: %w", err)
	}

	defer Close("media ttninjs body", res.Body, &outError)

	switch res.StatusCode {
	case http.StatusOK:
	case http.StatusNotFound:
		return ttninjs.Document{}, fmt.Errorf(
			"document could not be found: %w",
			TTNINJSPermanentError{
				Cause: TTNINJSErrorCauseNotFound,
			})
	default:
		return ttninjs.Document{}, fmt.Errorf(
			"media API responded with: %s", res.Status)
	}

	payload, err := io.ReadAll(res.Body)
	if err != nil {
		return ttninjs.Document{}, fmt.Errorf(
			"read media response: %w", err)
	}

	var doc ttninjs.Document

	err = jsoniter.Unmarshal(payload, &doc)
	if err != nil {
		return ttninjs.Document{}, fmt.Errorf(
			"unmarshal document: %w",
			errors.Join(err, TTNINJSPermanentError{
				Cause: TTNINJSErrorCauseInvalidDoc,
			}))
	}

	return doc, nil
}
