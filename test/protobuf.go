package test

import (
	"bytes"
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
	JSONTransform(value map[string]any) error
}

type GoldenHelperForAny interface {
	JSONTransformAny(value any) error
}

var _ GoldenHelper = IgnoreField[string]{}

type IgnoreField[T any] struct {
	Name       string
	DummyValue T
	Validator  func(v T) error
}

// CmpOpts implements GoldenHelper.
func (fi IgnoreField[T]) CmpOpts() cmp.Options {
	return cmp.Options{
		cmpopts.IgnoreMapEntries(func(k string, _ any) bool {
			return k == fi.Name
		}),
	}
}

// JSONTransform implements GoldenHelper.
func (fi IgnoreField[T]) JSONTransform(value map[string]any) error {
	return keyReplacement(value, func(m map[string]any, k string) error {
		current, ok := m[fi.Name]
		if !ok {
			return nil
		}

		cast, ok := current.(T)
		if !ok {
			return fmt.Errorf("wrong type %T for %q", current, fi.Name)
		}

		if fi.Validator != nil {
			err := fi.Validator(cast)
			if err != nil {
				return fmt.Errorf("validate original value: %w", err)
			}
		}

		m[fi.Name] = fi.DummyValue

		return nil
	})
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

func (it IgnoreTimestamps) JSONTransform(value map[string]any) error {
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

	return keyReplacement(value, func(m map[string]any, k string) error {
		if !slices.Contains(fields, k) {
			return nil
		}

		m[k] = dummy

		return nil
	})
}

func keyReplacement(
	value map[string]any,
	fn func(m map[string]any, k string) error,
) error {
	var (
		tSlice func(s []any) error
		tMap   func(m map[string]any) error
	)

	tSlice = func(s []any) error {
		for i := range s {
			switch v := s[i].(type) {
			case []any:
				err := tSlice(v)
				if err != nil {
					return err
				}
			case map[string]any:
				err := tMap(v)
				if err != nil {
					return err
				}
			}
		}

		return nil
	}

	tMap = func(m map[string]any) error {
		for k := range m {
			err := fn(m, k)
			if err != nil {
				return err
			}

			switch v := m[k].(type) {
			case []any:
				err := tSlice(v)
				if err != nil {
					return err
				}
			case map[string]any:
				err := tMap(v)
				if err != nil {
					return err
				}
			}
		}

		return nil
	}

	return tMap(value)
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

	// Clone the message so that we don't affect our source data.
	got = proto.Clone(got)

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
			err := helpers[i].JSONTransform(obj)
			Must(t, err, "transform message for storage")
		}

		data, err = json.Marshal(obj)
		Must(t, err, "marshal message for roundtrip in %q", goldenPath)

		proto.Reset(got)

		err = protojson.Unmarshal(data, got)
		Must(t, err, "roundtrip back to proto message")

		data, err = opts.Marshal(got)
		Must(t, err, "marshal roundtripped proto message")

		var buf bytes.Buffer

		// Indent output because of this tomfoolery:
		// https://github.com/golang/protobuf/issues/1121
		err = json.Indent(&buf, data, "", "  ")
		Must(t, err, "indent proto JSON")

		// End all files with a newline
		_ = buf.WriteByte('\n')

		err = os.WriteFile(goldenPath, buf.Bytes(), 0o600)
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
		cmpOpts = append(cmpOpts, h.CmpOpts()...)
	}

	EqualMessageWithOptions(t, wantMessage, got, cmpOpts, "must match golden file %q", goldenPath)
}
