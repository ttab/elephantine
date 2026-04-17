package elephantine

import (
	"encoding/json"
	"fmt"
	"net/http"
	"runtime/debug"
)

// BuildInfo is the payload returned by the /version endpoint.
type BuildInfo struct {
	Application ApplicationInfo   `json:"application"`
	Modules     map[string]string `json:"modules"`
}

// ApplicationInfo describes the running application binary.
type ApplicationInfo struct {
	Name        string `json:"name"`
	Version     string `json:"version"`
	VCSRevision string `json:"vcs_revision,omitempty"`
	VCSTime     string `json:"vcs_time,omitempty"`
	VCSModified bool   `json:"vcs_modified,omitempty"`
}

// devVersion is reported when neither the application nor the Go toolchain
// has stamped a real version into the binary.
const devVersion = "v0.0.0-dev"

// defaultVersionModules is the baseline set of modules whose versions are
// reported by /version. APIServerModules appends to this list.
var defaultVersionModules = []string{
	"github.com/ttab/elephantine",
	"github.com/ttab/elephant-api",
	"github.com/ttab/elephant-tt-api",
}

func buildBuildInfo(appVersion string, modules []string) BuildInfo {
	out := BuildInfo{
		Modules: make(map[string]string, len(modules)),
	}

	info, ok := debug.ReadBuildInfo()
	if !ok {
		out.Application.Version = resolveAppVersion(appVersion, "")

		return out
	}

	out.Application.Name = info.Main.Path
	out.Application.Version = resolveAppVersion(appVersion, info.Main.Version)

	for _, s := range info.Settings {
		switch s.Key {
		case "vcs.revision":
			out.Application.VCSRevision = s.Value
		case "vcs.time":
			out.Application.VCSTime = s.Value
		case "vcs.modified":
			out.Application.VCSModified = s.Value == "true"
		}
	}

	depVersions := make(map[string]string, len(info.Deps))
	for _, dep := range info.Deps {
		depVersions[dep.Path] = dep.Version
	}

	for _, m := range modules {
		if _, exists := out.Modules[m]; exists {
			continue
		}

		if m == info.Main.Path {
			out.Modules[m] = out.Application.Version

			continue
		}

		v, ok := depVersions[m]
		if !ok {
			// Module isn't part of this build; skip it rather than
			// reporting a fake "unknown" version.
			continue
		}

		out.Modules[m] = v
	}

	return out
}

// resolveAppVersion picks the application version to report. An explicit
// value from APIServerVersion wins; otherwise Main.Version is used when it
// looks like a real module version; otherwise we fall back to devVersion so
// the field always carries a meaningful string.
func resolveAppVersion(explicit string, mainVersion string) string {
	if explicit != "" {
		return explicit
	}

	if mainVersion != "" && mainVersion != "(devel)" {
		return mainVersion
	}

	return devVersion
}

func versionHandler(info BuildInfo) http.Handler {
	body, err := json.MarshalIndent(info, "", "  ")
	if err != nil {
		// Should be impossible — BuildInfo is pure strings. Fall back to a
		// handler that reports the error so the problem is visible.
		msg := fmt.Sprintf("marshal build info: %v", err)

		return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			http.Error(w, msg, http.StatusInternalServerError)
		})
	}

	body = append(body, '\n')

	return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)

		_, _ = w.Write(body)
	})
}

// bomHandler writes the full debug.BuildInfo as plain text in the canonical
// `go version -m` format. Intended to be served on the internal health
// server only.
func bomHandler(w http.ResponseWriter, _ *http.Request) {
	info, ok := debug.ReadBuildInfo()
	if !ok {
		http.Error(w, "build info not available", http.StatusInternalServerError)

		return
	}

	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(http.StatusOK)

	_, _ = fmt.Fprint(w, info.String())
}
