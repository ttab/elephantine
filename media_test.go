package elephantine_test

import (
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"testing"

	"github.com/ttab/elephantine"
	"github.com/ttab/elephantine/test"
)

func TestMedia(t *testing.T) {
	ctx := t.Context()
	logger := slog.New(test.NewLogHandler(t, slog.LevelWarn))

	server := NewMediaMockServer(t)

	server.AddDocument(t,
		"/media/text/231107-oktobervader-1d76cbf0.json",
		"./testdata/ttninjs.weather.rendered.json")

	media := elephantine.NewMedia(logger, server.Client(), server.Host())

	doc, err := media.GetRenderedTTNINJS(
		ctx, "http://tt.se/media/text/231107-oktobervader-1d76cbf0", nil)
	test.Must(t, err, "get rendered TTNINJS")

	test.Equal(t, "Kallaste oktober i norr på 30 år", doc.Headline,
		"get the correct headline")

	a001, ok := doc.Associations["a001"]
	if !ok {
		t.Fatal("expected rendered document to have the association a001")
	}

	if len(a001.Renditions) == 0 {
		t.Fatal("expected rendered document to have renditions of a001")
	}
}

type MediaMockServer struct {
	server    *httptest.Server
	host      string
	documents map[string][]byte
}

func NewMediaMockServer(t *testing.T) *MediaMockServer {
	t.Helper()

	ms := MediaMockServer{
		documents: make(map[string][]byte),
	}

	ms.server = httptest.NewTLSServer(&ms)

	t.Cleanup(func() {
		ms.server.Close()
	})

	u, err := url.Parse(ms.server.URL)
	test.Must(t, err, "parse media mock server URL")

	ms.host = u.Host

	return &ms
}

func (ms *MediaMockServer) Host() string {
	return ms.host
}

func (ms *MediaMockServer) Client() *http.Client {
	return ms.server.Client()
}

func (ms *MediaMockServer) AddDocument(
	t *testing.T, path string, filename string,
) {
	t.Helper()

	data, err := os.ReadFile(filename)
	test.Must(t, err, "load media mock file")

	ms.documents[path] = data
}

func (ms *MediaMockServer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	data, ok := ms.documents[r.URL.Path]
	if !ok {
		w.WriteHeader(http.StatusNotFound)

		return
	}

	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write(data)
}
