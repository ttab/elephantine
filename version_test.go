package elephantine_test

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"testing"

	"github.com/ttab/elephantine"
	"github.com/ttab/elephantine/test"
)

type fetchResult struct {
	StatusCode int
	Header     http.Header
	Body       []byte
}

func fetch(t *testing.T, client *http.Client, url string) fetchResult {
	t.Helper()

	req, err := http.NewRequestWithContext(
		context.Background(), http.MethodGet, url, nil,
	)
	test.Mustf(t, err, "build request for %s", url)

	res, err := client.Do(req)
	test.Mustf(t, err, "GET %s", url)

	body, err := io.ReadAll(res.Body)
	test.Mustf(t, err, "read body from %s", url)

	err = res.Body.Close()
	test.Mustf(t, err, "close body from %s", url)

	return fetchResult{
		StatusCode: res.StatusCode,
		Header:     res.Header,
		Body:       body,
	}
}

func TestAPIServerVersionEndpoint(t *testing.T) {
	logger := slog.New(test.NewLogHandler(t, slog.LevelDebug))

	srv, client := elephantine.NewTestAPIServer(t, logger,
		elephantine.APIServerVersion("v0.0.0-test"),
		elephantine.APIServerModules("github.com/prometheus/client_golang"),
	)

	err := srv.ListenAndServe(context.Background())
	test.Mustf(t, err, "start test API server")

	res := fetch(t, client, "http://"+srv.Addr()+"/version")

	test.Equalf(t, http.StatusOK, res.StatusCode,
		"status for GET /version")
	test.Equalf(t, "application/json", res.Header.Get("Content-Type"),
		"content type for GET /version")

	var info elephantine.BuildInfo

	err = json.Unmarshal(res.Body, &info)
	test.Mustf(t, err, "decode BuildInfo")

	test.Equalf(t, "v0.0.0-test", info.Application.Version,
		"application version reflects APIServerVersion")

	if info.Application.Name == "" {
		t.Fatal("expected non-empty application name")
	}

	// elephantine is the test's own module, so its entry is reported via
	// Main.Version (overridden here by APIServerVersion).
	test.Equalf(t, "v0.0.0-test",
		info.Modules["github.com/ttab/elephantine"],
		"elephantine module version tracks the application version")

	// Whether the test binary carries dependency versions at all differs
	// between Go toolchains, so client_golang may or may not be reported
	// here. Either is correct; reporting it with an empty version is not.
	// TestBuildInfoFrom covers the mapping itself against known build
	// information.
	for m, v := range info.Modules {
		if v == "" {
			t.Errorf("module %q reported with an empty version", m)
		}
	}

	// The elephant API modules aren't dependencies of elephantine, so they
	// can never be part of this build and are skipped rather than reported
	// as "unknown", even though they are in the default module list.
	for _, m := range []string{
		"github.com/ttab/elephant-api",
		"github.com/ttab/elephant-tt-api",
	} {
		if v, ok := info.Modules[m]; ok {
			t.Errorf("module %q unexpectedly present: %q", m, v)
		}
	}
}

func TestAPIServerVersionEndpointDefault(t *testing.T) {
	logger := slog.New(test.NewLogHandler(t, slog.LevelDebug))

	srv, client := elephantine.NewTestAPIServer(t, logger)

	err := srv.ListenAndServe(context.Background())
	test.Mustf(t, err, "start test API server")

	res := fetch(t, client, "http://"+srv.Addr()+"/version")

	var info elephantine.BuildInfo

	err = json.Unmarshal(res.Body, &info)
	test.Mustf(t, err, "decode BuildInfo")

	test.Equalf(t, "v0.0.0-dev", info.Application.Version,
		"default application version when APIServerVersion is not set")
}

func TestHealthServerBOMEndpoint(t *testing.T) {
	logger := slog.New(test.NewLogHandler(t, slog.LevelDebug))

	srv, _ := elephantine.NewTestAPIServer(t, logger)

	err := srv.ListenAndServe(context.Background())
	test.Mustf(t, err, "start test API server")

	res := fetch(t, http.DefaultClient,
		"http://"+srv.Health.Addr()+"/debug/bom")

	test.Equalf(t, http.StatusOK, res.StatusCode,
		"status for GET /debug/bom")

	ct := res.Header.Get("Content-Type")
	if !strings.HasPrefix(ct, "text/plain") {
		t.Errorf("expected text/plain content type, got %q", ct)
	}

	// Under `go test`, debug.BuildInfo.Deps is empty, so we can't rely on a
	// "dep\t" line. The main module line is always present.
	if !strings.Contains(string(res.Body), "mod\tgithub.com/ttab/elephantine") {
		t.Error("expected BOM to list the main module line")
	}
}
