package elephantine

import (
	"context"
	"errors"
	"fmt"

	"github.com/urfave/cli/v2"
)

// ParameterSource should be implemented to support loading of configuration
// paramaters that should be resolved at run time rather than given as
// environment variables or flags for the application. This is useful for
// loading secrets.
type ParameterSource interface {
	GetParameterValue(ctx context.Context, name string) (string, error)
}

type noParameterSource struct{}

func (noParameterSource) GetParameterValue(_ context.Context, _ string) (string, error) {
	return "", errors.New("no parameter source configured")
}

// GetParameterSource returns a named parameter source.
func GetParameterSource(name string) (ParameterSource, error) {
	switch name {
	case "":
		return noParameterSource{}, nil
	case "ssm":
		return NewLazySSM(), nil
	default:
		return nil, fmt.Errorf("unknown parameter source %q", name)
	}
}

// ResolveParameter loads the parameter from the parameter source if
// "[name]-parameter" has been set for the cli.Context, otherwise the value of
// "[name]" will be returned.
func ResolveParameter(
	ctx context.Context, c *cli.Context, src ParameterSource, name string,
) (string, error) {
	paramName := c.String(name + "-parameter")
	if paramName == "" {
		return c.String(name), nil
	}

	value, err := src.GetParameterValue(ctx, paramName)
	if err != nil {
		return "", fmt.Errorf("failed to fetch %q (%s) parameter value: %w",
			paramName, name, err)
	}

	return value, nil
}
