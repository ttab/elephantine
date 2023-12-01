package elephantine

import (
	"context"
	"fmt"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/ssm"
)

// NewLazySSM creates a new SSM ParameterSource.
func NewLazySSM() *LazySSM {
	return &LazySSM{}
}

// NewLazySSM is a SSM-backed ParameterSource implementation for
// ResolveParameter().
type LazySSM struct {
	ssm *ssm.Client
}

// GetParameterValue implements ParameterSource.
func (l *LazySSM) GetParameterValue(ctx context.Context, name string) (string, error) {
	if l.ssm == nil {
		cfg, err := config.LoadDefaultConfig(ctx,
			config.WithRegion("auto"),
		)
		if err != nil {
			return "", fmt.Errorf("failed to load AWS SDK config: %w", err)
		}

		l.ssm = ssm.NewFromConfig(cfg)
	}

	param, err := l.ssm.GetParameter(ctx, &ssm.GetParameterInput{
		Name:           aws.String(name),
		WithDecryption: aws.Bool(true),
	})
	if err != nil {
		return "", fmt.Errorf("error response from AWS SSM: %w", err)
	}

	return *param.Parameter.Value, nil
}
