package elephantine

import (
	"context"
	"io"
	"os"

	"golang.org/x/exp/slog"
)

// Log attribute keys used throughout the application.
const (
	// LogKeyLogLevel is the log level that an application was configured
	// with.
	LogKeyLogLevel = "log_level"
	// LogKeyError is an error message.
	LogKeyError = "err"
	// LogKeyErrorCode is an error code.
	LogKeyErrorCode = "err_code"
	// LogKeyErrorMeta is a JSON object with error metadata.
	LogKeyErrorMeta = "err_meta"
	// LogKeyCountMetric was planned to be used to increment a given metric
	// when used. TODO: not implemented yet, should it be removed?
	LogKeyCountMetric = "count_metric"
	// LogKeyDocumentUUID is the UUID of a document.
	LogKeyDocumentUUID = "document_uuid"
	// LogKeyDocumentVersion is the version of a document.
	LogKeyDocumentVersion = "document_version"
	// LogKeyDocumentStatus is the status of a document.
	LogKeyDocumentStatus = "document_status"
	// LogKeyDocumentStatus is the id of a document status.
	LogKeyDocumentStatusID = "document_status_id"
	// LogKeyTransaction is the name of a transaction, usually used to
	// identify a transaction that has failed.
	LogKeyTransaction = "transaction"
	// LogKeyOCSource is used to identify a source document from OC by UUID.
	LogKeyOCSource = "oc_source"
	// LogKeyOCVersion is the version of the OC document.
	LogKeyOCVersion = "oc_version"
	// LogKeyOCEvent is the type of an OC event- or content-log event.
	LogKeyOCEvent = "oc_event"
	// LogKeyChannel identifies a notification channel.
	LogKeyChannel = "channel"
	// LogKeyMessage can be used to log a unexpected message.
	LogKeyMessage = "message"
	// LogKeyDelay can be used to communicate the delay when logging
	// information about retry attempts and backoff delays.
	LogKeyDelay = "delay"
	// LogKeyBucket is used to log a S3 bucket name.
	LogKeyBucket = "bucket"
	// LogKeyObjectKey is used to log a S3 object key.
	LogKeyObjectKey = "object_key"
	// LogKeyComponent is used to communicate what application subcomponent
	// the log entry is from.
	LogKeyComponent = "component"
	// LogKeyCount is used to communicate a count.
	LogKeyCount = "count"
	// LogKeyEventID is the ID of an event.
	LogKeyEventID = "event_id"
	// LogKeyEventType is the type of an event.
	LogKeyEventType = "event_type"
	// LogKeyJobLock is the name of a job lock.
	LogKeyJobLock = "job_lock"
	// LogKeyJobLockID is the ID of a job lock.
	LogKeyJobLockID = "job_lock_id"
	// LogKeyState is the name of a state, like "held", "lost" or "accepted".
	LogKeyState = "state"
	// LogKeyIndex is the name of a search index, like an Open Search index.
	LogKeyIndex = "index"
	// LogKeyRoute is used to name a route or path.
	LogKeyRoute = "route"
	// LogKeyService is used to specify an RPC service.
	LogKeyService = "service"
	// LogKeyMethod is used to specify an RPC method.
	LogKeyMethod = "method"
	// LogKeySubject is the sub of an authenticated client.
	LogKeySubject = "sub"
	// LogKeyScopes are the scopes of the authenticated client.
	LogKeyScopes = "scopes"
	// LogKeyStatusCode is the HTTP status code used for a response.
	LogKeyStatusCode = "status_code"
	// LogKeyName is the name of a resource.
	LogKeyName = "name"
)

// SetUpLogger creates a default JSON logger and sets it as the global logger.
func SetUpLogger(logLevel string, w io.Writer) *slog.Logger {
	logger := slog.New(slog.NewJSONHandler(w, nil))

	level := slog.LevelWarn

	if logLevel != "" {
		err := level.UnmarshalText([]byte(logLevel))
		if err != nil {
			level = slog.LevelWarn

			logger.Error("invalid log level",
				LogKeyError, err,
				LogKeyLogLevel, logLevel)
		}
	}

	handler := &contextHandler{
		h: slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{
			Level: &level,
		}),
	}

	logger = slog.New(handler)

	slog.SetDefault(logger)

	return logger
}

type ctxKey int

const logCtxKey ctxKey = 1

// WithLogMetadata creates a child context with a log metadata map.
func WithLogMetadata(ctx context.Context) context.Context {
	m := make(map[string]any)

	return context.WithValue(ctx, logCtxKey, m)
}

// GetLogMetadata returns the log metatada map for the context.
func GetLogMetadata(ctx context.Context) map[string]any {
	m, ok := ctx.Value(logCtxKey).(map[string]any)
	if !ok {
		return nil
	}

	return m
}

// SetLogMetadata sets a log metadata value on the context if it has a log
// metadata map.
func SetLogMetadata(ctx context.Context, key string, value any) {
	m, ok := ctx.Value(logCtxKey).(map[string]any)
	if !ok {
		return
	}

	m[key] = value
}

type contextHandler struct {
	h slog.Handler
}

func (h *contextHandler) Enabled(ctx context.Context, level slog.Level) bool {
	return h.h.Enabled(ctx, level)
}

func (h *contextHandler) Handle(ctx context.Context, r slog.Record) error {
	if !h.h.Enabled(ctx, r.Level) {
		return nil
	}

	m := GetLogMetadata(ctx)

	for k, v := range m {
		r.Add(k, v)
	}

	return h.h.Handle(ctx, r)
}

func (h *contextHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	ah := h.h.WithAttrs(attrs)

	return &contextHandler{h: ah}
}

func (h *contextHandler) WithGroup(name string) slog.Handler {
	gh := h.h.WithGroup(name)

	return &contextHandler{h: gh}
}
