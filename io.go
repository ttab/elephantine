package elephantine

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
)

// UnmarshalFile is a utility function for reading and unmarshalling a file
// containing JSON. The parsing will be strict and disallow unknown fields.
func UnmarshalFile(path string, o any) (outErr error) {
	f, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("failed to open file: %w", err)
	}

	defer func() {
		err := f.Close()
		if err != nil {
			outErr = errors.Join(outErr, fmt.Errorf(
				"failed to close file: %w", err))
		}
	}()

	dec := json.NewDecoder(f)

	dec.DisallowUnknownFields()

	err = dec.Decode(o)
	if err != nil {
		return fmt.Errorf("failed to unmarshal JSON: %w", err)
	}

	return nil
}

// MarshalToFile is a utility function for marshalling a data structore to JSON
// and write it to a fil. The JSON will be pretty printed.
func MarshalFile(path string, o any) (outErr error) {
	f, err := os.Create(path)
	if err != nil {
		return fmt.Errorf("failed to create file: %w", err)
	}

	defer func() {
		err := f.Close()
		if err != nil {
			outErr = errors.Join(outErr, fmt.Errorf(
				"failed to close file: %w", err))
		}
	}()

	dec := json.NewEncoder(f)
	dec.SetIndent("", "  ")

	err = dec.Encode(o)
	if err != nil {
		return fmt.Errorf("failed to marshal JSON: %w", err)
	}

	return nil
}

// UnmarshalHTTPResource is a utility function for reading and unmarshalling a
// HTTP resource. Uses the default HTTP client.
func UnmarshalHTTPResource(resURL string, o any) (outErr error) {
	res, err := http.Get(resURL) //nolint:gosec
	if err != nil {
		return fmt.Errorf("failed to perform request: %w", err)
	}

	defer func() {
		err := res.Body.Close()
		if err != nil {
			outErr = errors.Join(outErr, fmt.Errorf(
				"failed to close response body: %w", err))
		}
	}()

	if res.StatusCode != http.StatusOK {
		return fmt.Errorf("server responded with: %q", res.Status)
	}

	dec := json.NewDecoder(res.Body)

	err = dec.Decode(o)
	if err != nil {
		return fmt.Errorf("failed to unmarshal JSON: %w", err)
	}

	return nil
}

// SafeClose can be used with defer to defer the Close of a resource without
// ignoring the error.
func SafeClose(logger *slog.Logger, name string, c io.Closer) {
	err := c.Close()
	if err != nil {
		logger.Error("failed to close",
			LogKeyName, name,
			LogKeyError, err.Error())
	}
}
