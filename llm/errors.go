package llm

import (
	"context"
	"errors"
	"time"

	"github.com/compforge/agentgo"
	"github.com/voocel/litellm"
)

// providerError adapts a litellm error to the agentgo kernel's
// provider-agnostic error contract. It lets the kernel read retry and
// classification facts (agentgo.RetryableError / RetryHinter, and the
// ErrContextOverflow / ErrProvider* sentinels via errors.Is) without importing
// litellm. Unwrap exposes the original error, so callers that DO know litellm
// can still match it with errors.As(&litellm.Error{}).
type providerError struct{ err error }

// wrapProviderError wraps a raw litellm error for kernel consumption. Control
// signals (context cancellation / deadline) are returned unchanged: the loop
// matches them directly with errors.Is, and classifying them as provider
// failures would wrongly flip their retryable bit.
func wrapProviderError(err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return err
	}
	return &providerError{err: err}
}

func (e *providerError) Error() string             { return e.err.Error() }
func (e *providerError) Unwrap() error             { return e.err }
func (e *providerError) Retryable() bool           { return litellm.IsTemporaryError(e.err) }
func (e *providerError) RetryAfter() time.Duration { return litellm.RetryAfter(e.err) }

// Is maps litellm's category checks onto the kernel's sentinels so callers can
// classify provider failures with a plain errors.Is. Other targets fall through
// to the unwrapped litellm error's own matching.
func (e *providerError) Is(target error) bool {
	switch target {
	case agentgo.ErrContextOverflow:
		return litellm.ErrorTypeOf(e.err) == litellm.ErrorTypeContextOverflow
	case agentgo.ErrProviderRateLimit:
		return litellm.ErrorTypeOf(e.err) == litellm.ErrorTypeRateLimit
	case agentgo.ErrProviderQuota:
		return isQuotaError(e.err)
	case agentgo.ErrProviderTimeout:
		return litellm.ErrorTypeOf(e.err) == litellm.ErrorTypeTimeout
	case agentgo.ErrProviderStreamIdle:
		return agentgo.IsStreamIdleMessage(e.err.Error())
	case agentgo.ErrProviderNetwork:
		return litellm.ErrorTypeOf(e.err) == litellm.ErrorTypeNetwork
	case agentgo.ErrProviderAuth:
		return litellm.ErrorTypeOf(e.err) == litellm.ErrorTypeAuth
	case agentgo.ErrProviderOverloaded:
		return litellm.ErrorTypeOf(e.err) == litellm.ErrorTypeOverloaded
	case agentgo.ErrProviderContentFilter:
		return litellm.ErrorTypeOf(e.err) == litellm.ErrorTypeContentFilter
	}
	return false
}

func isQuotaError(err error) bool {
	var e *litellm.Error
	return errors.As(err, &e) && e.Type == litellm.ErrorTypeQuota
}
