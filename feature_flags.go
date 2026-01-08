package elephantine

import "context"

var ctxKeyFlags int

// ContextWithFeatureFlags creates a context with the specified context flags
// set. If the context already has feature flags set they will be preserved as
// is unless overridden by the new flags.
func ContextWithFeatureFlags(ctx context.Context, flags map[string]bool) context.Context {
	previous, ok := ctx.Value(&ctxKeyFlags).(map[string]bool)
	if ok && previous != nil {
		// Copy over existing feature flag unless they are set in the
		// new flags.
		for k := range previous {
			_, isSet := flags[k]
			if isSet {
				continue
			}

			flags[k] = previous[k]
		}
	}

	return context.WithValue(ctx, &ctxKeyFlags, flags)
}

// FeatureIsEnabled checks the state of a feature flag.
func FeatureIsEnabled(ctx context.Context, flag string, defaultState bool) bool {
	val, ok := ctx.Value(&ctxKeyFlags).(map[string]bool)
	if !ok || val == nil {
		return defaultState
	}

	state, ok := val[flag]
	if !ok {
		return defaultState
	}

	return state
}
