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

type TestingT interface {
	Helper()
	Fatalf(format string, args ...any)
	Logf(format string, args ...any)
}

func Must(t TestingT, err error, format string, a ...any) {
	t.Helper()

	if err != nil {
		t.Fatalf("failed: %s: %v", fmt.Sprintf(format, a...), err)
	}

	if debug() {
		t.Logf("success: "+format, a...)
	}
}

func MustNot(t TestingT, err error, format string, a ...any) {
	t.Helper()

	if err == nil {
		t.Fatalf("failed: %s", fmt.Sprintf(format, a...))
	}

	if debug() {
		msg := fmt.Sprintf(format, a...)
		t.Logf("success: %s: error message: %v", msg, err)
	}
}

func NotNil[T any](t TestingT, v *T, format string, a ...any) {
	t.Helper()

	if v == nil {
		t.Fatalf("failed: %s", fmt.Sprintf(format, a...))
	}

	if debug() {
		t.Logf("success: "+format, a...)
	}
}

func Equal[T comparable](t TestingT, want T, got T, format string, a ...any) {
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

// TestAgainstGolden compares a result against the contents of the file at the
// goldenPath. Run with regenerate set to true to create or update the file.
func TestAgainstGolden[T any](
	t *testing.T,
	regenerate bool,
	got T,
	goldenPath string,
	helpers ...GoldenHelper,
) {
	t.Helper()

	if regenerate {
		data, err := json.Marshal(got)
		Must(t, err, "marshal result")

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
		Must(t, err, "unmarshal for transform")

		for i := range helpers {
			anyHelper, hasAnyHelper := helpers[i].(GoldenHelperForAny)

			switch {
			case objMap != nil:
				err := helpers[i].JSONTransform(objMap)
				Must(t, err, "transform for storage")
			case hasAnyHelper:
				err := anyHelper.JSONTransformAny(obj)
				Must(t, err, "transform for storage")
			}
		}

		data, err = json.MarshalIndent(obj, "", "  ")
		Must(t, err, "marshal for storage in %q", goldenPath)

		// End all files with a newline
		data = append(data, '\n')

		err = os.WriteFile(goldenPath, data, 0o600)
		Must(t, err, "write golden file %q", goldenPath)
	}

	wantData, err := os.ReadFile(goldenPath)
	Must(t, err, "read from golden file %q", goldenPath)

	var wantValue T

	err = json.Unmarshal(wantData, &wantValue)
	Must(t, err, "unmarshal data from golden file %q", goldenPath)

	var cmpOpts cmp.Options

	for _, h := range helpers {
		cmpOpts = append(cmpOpts, h.CmpOpts()...)
	}

	EqualDiffWithOptions(t, wantValue, got, cmpOpts,
		"must match golden file %q", goldenPath)
}

// EqualMessage runs a cmp.Diff with protobuf-specific options.
func EqualDiffWithOptions[T any](
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
