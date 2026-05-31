// SPDX-License-Identifier: Apache-2.0
// Copyright Authors of Proofhouse

package pkgsite

import (
	"net/http"
	"time"
)

// This file exposes unexported internals to the black-box test package,
// the standard export_test.go pattern. The symbols exist only in the
// test binary, so they don't widen the real API.

// NewRetryTransport exposes newRetryTransport so tests can wrap an
// in-memory RoundTripper and exercise the retry/backoff policy.
func NewRetryTransport(
	inner http.RoundTripper,
	initialDelay, maxDelay, attemptTimeout time.Duration,
) http.RoundTripper {
	return newRetryTransport(inner, initialDelay, maxDelay, attemptTimeout)
}

const (
	RetryMaxRetries     = retryMaxRetries
	RetryInitialDelay   = retryInitialDelay
	RetryMaxDelay       = retryMaxDelay
	RetryAttemptTimeout = defaultTimeout
)
