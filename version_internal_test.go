package elephantine

import (
	"runtime/debug"
	"testing"
)

// Module paths used by the build info tests.
const (
	mainModule      = "github.com/ttab/elephant-service"
	libModule       = "github.com/ttab/elephantine"
	depModule       = "github.com/prometheus/client_golang"
	notBuiltModule  = "github.com/ttab/elephant-api"
	testMainVersion = "v1.2.3"
	testLibVersion  = "v0.27.4"
	testDepVersion  = "v1.24.1"
)

// TestBuildInfoFrom covers the mapping from build information to the
// /version payload against known input. It is an internal test because the
// build information of the test binary itself is not something we can rely
// on: whether it carries dependency versions at all differs between Go
// toolchains.
func TestBuildInfoFrom(t *testing.T) {
	info := &debug.BuildInfo{
		Main: debug.Module{
			Path:    mainModule,
			Version: develMainVersion,
		},
		Deps: []*debug.Module{
			{
				Path:    libModule,
				Version: testLibVersion,
			},
			{
				Path:    depModule,
				Version: testDepVersion,
			},
		},
		Settings: []debug.BuildSetting{
			{Key: "vcs.revision", Value: "cafebabe"},
			{Key: "vcs.time", Value: "2026-08-25T06:00:00Z"},
			{Key: "vcs.modified", Value: "true"},
		},
	}

	modules := []string{
		mainModule,
		libModule,
		depModule,
		notBuiltModule,
	}

	out := buildInfoFrom(info, testMainVersion, modules)

	if out.Application.Name != mainModule {
		t.Errorf("unexpected application name: %q", out.Application.Name)
	}

	// An explicit version from APIServerVersion wins over Main.Version.
	if out.Application.Version != testMainVersion {
		t.Errorf("unexpected application version: %q", out.Application.Version)
	}

	if out.Application.VCSRevision != "cafebabe" {
		t.Errorf("unexpected VCS revision: %q", out.Application.VCSRevision)
	}

	if out.Application.VCSTime != "2026-08-25T06:00:00Z" {
		t.Errorf("unexpected VCS time: %q", out.Application.VCSTime)
	}

	if !out.Application.VCSModified {
		t.Error("expected the build to be reported as modified")
	}

	// The main module is reported with the application version, and
	// dependencies with the versions they were built with.
	want := map[string]string{
		mainModule: testMainVersion,
		libModule:  testLibVersion,
		depModule:  testDepVersion,
	}

	for m, v := range want {
		if out.Modules[m] != v {
			t.Errorf("module %q: expected version %q, got %q",
				m, v, out.Modules[m])
		}
	}

	// A requested module that isn't part of the build is skipped rather
	// than reported with a fake version.
	if v, ok := out.Modules[notBuiltModule]; ok {
		t.Errorf("expected a module outside the build to be skipped, got %q", v)
	}
}

// TestBuildInfoFromWithoutBuildInfo verifies the fallback for a binary
// built without module support, where no build information is available.
func TestBuildInfoFromWithoutBuildInfo(t *testing.T) {
	out := buildInfoFrom(nil, "", defaultVersionModules)

	if out.Application.Version != devVersion {
		t.Errorf("expected the fallback version %q, got %q",
			devVersion, out.Application.Version)
	}

	if len(out.Modules) != 0 {
		t.Errorf("expected no modules to be reported, got %v", out.Modules)
	}
}

// TestBuildInfoFromDevelMainVersion verifies that the "(devel)" version the
// toolchain stamps into an unversioned build isn't reported as if it were a
// real version.
func TestBuildInfoFromDevelMainVersion(t *testing.T) {
	info := &debug.BuildInfo{
		Main: debug.Module{
			Path:    mainModule,
			Version: develMainVersion,
		},
	}

	out := buildInfoFrom(info, "", []string{mainModule})

	if out.Application.Version != devVersion {
		t.Errorf("expected the fallback version %q, got %q",
			devVersion, out.Application.Version)
	}

	if out.Modules[mainModule] != devVersion {
		t.Errorf("expected the main module to track the application version, got %q",
			out.Modules[mainModule])
	}
}
