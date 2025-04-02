package test

import (
	"encoding/json"
	"fmt"
	"os"
	"reflect"
	"slices"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
	"github.com/google/go-cmp/cmp/cmpopts"
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

type GoldenHelper interface {
	CmpOpts() cmp.Options
	JSONTransform(value map[string]any)
}

func CommonTimeFields() []string {
	return []string{"timestamp", "modified", "created", "updated", "created_at", "updated_at"}
}

type IgnoreTimestamps struct {
	Fields     []string
	DummyValue time.Time
	Format     string
}

func (it IgnoreTimestamps) CmpOpts() cmp.Options {
	fields := it.Fields

	if len(fields) == 0 {
		fields = CommonTimeFields()
	}

	return cmp.Options{
		cmpopts.IgnoreMapEntries(func(k string, _ any) bool {
			return slices.Contains(fields, k)
		}),
	}
}

func (it IgnoreTimestamps) JSONTransform(value map[string]any) {
	fields := it.Fields

	if len(fields) == 0 {
		fields = CommonTimeFields()
	}

	format := it.Format
	if format == "" {
		format = time.RFC3339
	}

	t := it.DummyValue
	if t.IsZero() {
		t = time.Date(2023, time.January, 10, 20, 6, 37, 0,
			time.FixedZone("Europe/Stockholm", 3600))
	}

	dummy := t.Format(format)

	var (
		tSlice func(s []any)
		tMap   func(m map[string]any)
	)

	tSlice = func(s []any) {
		for i := range s {
			switch v := s[i].(type) {
			case []any:
				tSlice(v)
			case map[string]any:
				tMap(v)
			}
		}
	}

	tMap = func(m map[string]any) {
		for k := range m {
			switch v := m[k].(type) {
			case []any:
				tSlice(v)
			case map[string]any:
				tMap(v)
			default:
				if !slices.Contains(fields, k) {
					continue
				}

				m[k] = dummy
			}
		}
	}

	tMap(value)
}

// TestMessageAgainstGolden compares a protobuf message against the contents of
// the file at the goldenPath. Run with regenerate set to true to create or
// update the file.
func TestMessageAgainstGolden(
	t *testing.T,
	regenerate bool,
	got proto.Message,
	goldenPath string,
	helpers ...GoldenHelper,
) {
	t.Helper()

	if regenerate {
		opts := protojson.MarshalOptions{
			UseProtoNames: true,
			Multiline:     true,
			Indent:        "  ",
		}

		data, err := opts.Marshal(got)
		Must(t, err, "marshal proto message")

		var obj map[string]any

		err = json.Unmarshal(data, &obj)
		Must(t, err, "unmarshal message for transform")

		for i := range helpers {
			helpers[i].JSONTransform(obj)
		}

		data, err = json.MarshalIndent(obj, "", "  ")
		Must(t, err, "marshal message for storage in %q", goldenPath)

		// End all files with a newline
		data = append(data, '\n')

		err = os.WriteFile(goldenPath, data, 0o600)
		Must(t, err, "write golden file %q", goldenPath)
	}

	wantData, err := os.ReadFile(goldenPath)
	Must(t, err, "read from golden file %q", goldenPath)

	wantValue := reflect.New(reflect.TypeOf(got).Elem())
	wantMessage := wantValue.Interface().(proto.Message)

	err = protojson.Unmarshal(wantData, wantMessage)
	Must(t, err, "unmarshal data from golden file %q", goldenPath)

	var cmpOpts cmp.Options

	for _, h := range helpers {
		for _, opt := range h.CmpOpts() {
			cmpOpts = append(cmpOpts, opt)
		}
	}

	EqualMessageWithOptions(t, wantMessage, got, cmpOpts, "must match golden file %q", goldenPath)
}
