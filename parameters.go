package elephantine

import (
	"context"
	"fmt"

	"github.com/urfave/cli/v2"
)

type ParameterSource func(ctx context.Context, name string) (string, error)

func ResolveParameter(
	c *cli.Context, src ParameterSource, name string, defaultValue string,
) (string, error) {
	paramName := c.String(name)
	if paramName == "" {
		return defaultValue, nil
	}

	value, err := src(c.Context, paramName)
	if err != nil {
		return "", fmt.Errorf("failed to fetch %q (%s) parameter value: %w",
			paramName, name, err)
	}

	return value, nil
}
