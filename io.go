package elephantine

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"

	"golang.org/x/exp/slog"
)

func UnmarshalFile(path string, o interface{}) error {
	contents, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("failed to read file: %w", err)
	}

	dec := json.NewDecoder(bytes.NewReader(contents))

	dec.DisallowUnknownFields()

	err = dec.Decode(o)
	if err != nil {
		return fmt.Errorf("failed to unmarshal JSON: %w", err)
	}

	return nil
}

func UnmarshalHTTPResource(resURL string, o interface{}) error {
	res, err := http.Get(resURL) //nolint:gosec
	if err != nil {
		return fmt.Errorf("failed to perform request: %w", err)
	}

	defer func() {
		err := res.Body.Close()
		if err != nil {
			log.Printf("failed to close %q response body: %v",
				resURL, err)
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
