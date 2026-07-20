package elephantine_test

import (
	"context"
	"testing"

	"github.com/ttab/elephantine"
	"github.com/ttab/elephantine/test"
)

const (
	mcFlagFace = "mcface"
	mcFlagFoot = "mcfoot"
)

func TestContextWithFeatureFlags(t *testing.T) {
	ctx := t.Context()

	singleFeat := elephantine.ContextWithFeatureFlags(ctx, map[string]bool{
		mcFlagFace: true,
	})

	addedFeat := elephantine.ContextWithFeatureFlags(singleFeat, map[string]bool{
		mcFlagFoot: true,
	})

	removedFeat := elephantine.ContextWithFeatureFlags(addedFeat, map[string]bool{
		mcFlagFace: false,
	})

	t.Run("single feature", func(t *testing.T) {
		testFeatures(singleFeat, t, true, false)
	})

	t.Run("both features", func(t *testing.T) {
		testFeatures(addedFeat, t, true, true)
	})

	t.Run("one feature disabled", func(t *testing.T) {
		testFeatures(removedFeat, t, false, true)
	})
}

func testFeatures(
	ctx context.Context, t *testing.T,
	wantFace bool, wantFoot bool,
) {
	t.Helper()

	hasFace := elephantine.FeatureIsEnabled(ctx, mcFlagFace, false)
	test.Equalf(t, wantFace, hasFace, "face feature flag")

	hasFoot := elephantine.FeatureIsEnabled(ctx, mcFlagFoot, false)
	test.Equalf(t, wantFoot, hasFoot, "foot feature flag")
}
