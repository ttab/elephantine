package test

import (
	"fmt"
	"os"
	"reflect"
	"testing"

	"github.com/google/go-cmp/cmp"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/testing/protocmp"
)

// CloneMessage allows for type safe cloning of protobuf messages.
func CloneMessage[T proto.Message](msg T) T {
	return proto.Clone(msg).(T)
}

// EqualMessage runs a cmp.Diff with protobuf-specific options.
func EqualMessage(t TestingT,
	want proto.Message, got proto.Message,
	format string, a ...any,
) {
	t.Helper()

	EqualMessageWithOptions(t, want, got, nil, format, a...)
}

// EqualMessage runs a cmp.Diff with protobuf-specific options.
func EqualMessageWithOptions(t TestingT,
	want proto.Message, got proto.Message,
	opts cmp.Options,
	format string, a ...any,
) {
	t.Helper()

	o := cmp.Options{protocmp.Transform()}

	o = append(o, opts...)

	diff := cmp.Diff(want, got, o...)
	if diff != "" {
		msg := fmt.Sprintf(format, a...)
		t.Fatalf("%s: mismatch (-want +got):\n%s", msg, diff)
	}

	if testing.Verbose() {
		t.Logf("success: "+format, a...)
	}
}

// TestMessageAgainstGolden compares a protobuf message against the contents of
// the file at the goldenPath. Run with regenerate set to true to create or
// update the file.
func TestMessageAgainstGolden(
	t *testing.T,
	regenerate bool,
	got proto.Message,
	goldenPath string,
	opts ...cmp.Option,
) {
	t.Helper()

	if regenerate {
		opts := protojson.MarshalOptions{
			UseProtoNames: true,
			Multiline:     true,
			Indent:        "  ",
		}

		data, err := opts.Marshal(got)
		Must(t, err, "marshal message for storage in %q", goldenPath)

		err = os.WriteFile(goldenPath, data, 0o600)
		Must(t, err, "write golden file %q", goldenPath)
	}

	wantData, err := os.ReadFile(goldenPath)
	Must(t, err, "read from golden file %q", goldenPath)

	wantValue := reflect.New(reflect.TypeOf(got).Elem())
	wantMessage := wantValue.Interface().(proto.Message)

	err = protojson.Unmarshal(wantData, wantMessage)
	Must(t, err, "unmarshal data from golden file %q", goldenPath)

	EqualMessageWithOptions(t, wantMessage, got, opts, "must match golden file %q", goldenPath)
}
