package test

import (
	"encoding/json"
	"fmt"
	"os"
	"reflect"
	"sync"
	"testing"

	"github.com/google/go-cmp/cmp"
)

var (
	testDebug     bool
	readDebugOnce sync.Once
)

func debug() bool {
	readDebugOnce.Do(func() {
		testDebug = os.Getenv("TEST_DEBUG") == "true"
	})

	return testDebug
}

// Regenerate returns true if the environment variable REGENERATE is set to
// "true", signalling that golden test fixtures should be regenerated.
func Regenerate() bool {
	return os.Getenv("REGENERATE") == "true"
}

// TestingT is the subset of *testing.T used by the assertion helpers in this
// package, satisfied by both *testing.T and *testing.B.
type TestingT interface {
	Helper()
	Fatalf(format string, args ...any)
	Logf(format string, args ...any)
}

func Mustf(t TestingT, err error, format string, a ...any) {
	t.Helper()

	if err != nil {
		t.Fatalf("failed: %s: %v", fmt.Sprintf(format, a...), err)
	}

	if debug() {
		t.Logf("success: "+format, a...)
	}
}

// Must is a shim for [Mustf].
//
// Deprecated: use [Mustf] instead.
//
//go:fix inline
//nolint:goprintffuncname // deprecated shim.
func Must(t TestingT, err error, format string, a ...any) {
	Mustf(t, err, format, a...)
}

func MustNotf(t TestingT, err error, format string, a ...any) {
	t.Helper()

	if err == nil {
		t.Fatalf("failed: %s", fmt.Sprintf(format, a...))
	}

	if debug() {
		msg := fmt.Sprintf(format, a...)
		t.Logf("success: %s: error message: %v", msg, err)
	}
}

// MustNot is a shim for [MustNotf].
//
// Deprecated: use [MustNotf] instead.
//
//go:fix inline
//nolint:goprintffuncname // deprecated shim.
func MustNot(t TestingT, err error, format string, a ...any) {
	MustNotf(t, err, format, a...)
}

func NotNilf[T any](t TestingT, v *T, format string, a ...any) {
	t.Helper()

	if v == nil {
		t.Fatalf("failed: %s", fmt.Sprintf(format, a...))
	}

	if debug() {
		t.Logf("success: "+format, a...)
	}
}

// NotNil is a shim for [NotNilf].
//
// Deprecated: use [NotNilf] instead.
//
//go:fix inline
//nolint:goprintffuncname // deprecated shim.
func NotNil[T any](t TestingT, v *T, format string, a ...any) {
	NotNilf(t, v, format, a...)
}

func Equalf[T comparable](t TestingT, want T, got T, format string, a ...any) {
	t.Helper()

	diff := cmp.Diff(want, got)
	if diff != "" {
		t.Fatalf("failed: %s: mismatch (-want +got):\n%s",
			fmt.Sprintf(format, a...), diff)
	}

	if debug() {
		t.Logf("success: "+format, a...)
	}
}

// Equal is a shim for [Equalf].
//
// Deprecated: use [Equalf] instead.
//
//go:fix inline
//nolint:goprintffuncname // deprecated shim.
func Equal[T comparable](t TestingT, want T, got T, format string, a ...any) {
	Equalf(t, want, got, format, a...)
}

// EqualDiff runs a cmp.Diff to do a deep equal check with readable diff output.
func EqualDiff[T any](t TestingT,
	want T, got T,
	format string, a ...any,
) {
	t.Helper()

	diff := cmp.Diff(want, got)
	if diff != "" {
		msg := fmt.Sprintf(format, a...)
		t.Fatalf("%s: mismatch (-want +got):\n%s", msg, diff)
	}

	if debug() {
		t.Logf("success: "+format, a...)
	}
}

// AgainstGolden compares a result against the contents of the file at the
// goldenPath. Run with regenerate set to true to create or update the file.
func AgainstGolden[T any](
	t *testing.T,
	regenerate bool,
	got T,
	goldenPath string,
	helpers ...GoldenHelper,
) {
	t.Helper()

	if regenerate {
		data, err := json.Marshal(got)
		Mustf(t, err, "marshal result")

		var (
			obj    any
			objMap map[string]any
		)

		switch reflect.TypeOf(got).Kind() {
		case reflect.Array:
			obj = []any{}
		case reflect.Map:
		case reflect.Struct:
			objMap = map[string]any{}
			obj = objMap
		default:
			var z T

			obj = z
		}

		err = json.Unmarshal(data, &obj)
		Mustf(t, err, "unmarshal for transform")

		for i := range helpers {
			anyHelper, hasAnyHelper := helpers[i].(GoldenHelperForAny)

			switch {
			case objMap != nil:
				err := helpers[i].JSONTransform(objMap)
				Mustf(t, err, "transform for storage")
			case hasAnyHelper:
				err := anyHelper.JSONTransformAny(obj)
				Mustf(t, err, "transform for storage")
			}
		}

		data, err = json.MarshalIndent(obj, "", "  ")
		Mustf(t, err, "marshal for storage in %q", goldenPath)

		// End all files with a newline
		data = append(data, '\n')

		err = os.WriteFile(goldenPath, data, 0o600)
		Mustf(t, err, "write golden file %q", goldenPath)
	}

	wantData, err := os.ReadFile(goldenPath)
	Mustf(t, err, "read from golden file %q", goldenPath)

	var wantValue T

	err = json.Unmarshal(wantData, &wantValue)
	Mustf(t, err, "unmarshal data from golden file %q", goldenPath)

	var cmpOpts cmp.Options

	for _, h := range helpers {
		cmpOpts = append(cmpOpts, h.CmpOpts()...)
	}

	EqualDiffWithOptionsf(t, wantValue, got, cmpOpts,
		"must match golden file %q", goldenPath)
}

// TestAgainstGolden compares a result against the contents of the file at the
// goldenPath. Run with regenerate set to true to create or update the file.
//
// Deprecated: use [AgainstGolden] instead.
//
//go:fix inline
//nolint:revive // deprecated shim; a single call keeps it inlinable.
func TestAgainstGolden[T any](
	t *testing.T,
	regenerate bool,
	got T,
	goldenPath string,
	helpers ...GoldenHelper,
) {
	AgainstGolden(t, regenerate, got, goldenPath, helpers...)
}

func EqualDiffWithOptionsf[T any](
	t TestingT,
	want T, got T,
	opts cmp.Options,
	format string, a ...any,
) {
	t.Helper()

	diff := cmp.Diff(want, got, opts...)
	if diff != "" {
		msg := fmt.Sprintf(format, a...)
		t.Fatalf("%s: mismatch (-want +got):\n%s", msg, diff)
	}

	if debug() {
		t.Logf("success: "+format, a...)
	}
}

// EqualDiffWithOptions is a shim for [EqualDiffWithOptionsf].
//
// Deprecated: use [EqualDiffWithOptionsf] instead.
//
//go:fix inline
//nolint:goprintffuncname // deprecated shim.
func EqualDiffWithOptions[T any](
	t TestingT,
	want T, got T,
	opts cmp.Options,
	format string, a ...any,
) {
	EqualDiffWithOptionsf(t, want, got, opts, format, a...)
}
