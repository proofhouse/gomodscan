// SPDX-License-Identifier: Apache-2.0
// Copyright Authors of Proofhouse

// Package retryhttp builds the retry/backoff [http.RoundTripper] that the
// OSV and pkg.go.dev clients share. Both upstreams answer an occasional
// request with a 429, a transient 5xx, or a dropped connection, and a scan
// walks hundreds of modules, so one transient failure shouldn't end the run.
// [NewTransport] re-issues those attempts with exponential backoff and honors
// a Retry-After header when the upstream sends one. It leaves terminal
// responses alone, so each client's own status switch still maps a 404 or a
// 400 onto its own error.
package retryhttp

import (
	"net/http"
	"time"

	"github.com/failsafe-go/failsafe-go/failsafehttp"
	"github.com/failsafe-go/failsafe-go/timeout"
)

const (
	// MaxRetries bounds how many times the transport re-issues a request
	// after a retryable response (429 or most 5xx) or a transient network
	// error. Two keeps the worst-case per-request wait modest when an
	// upstream throttles the whole host.
	MaxRetries = 2

	// InitialDelay sets the first exponential-backoff step the transport
	// waits after a 429 that arrives without a Retry-After header, which the
	// upstreams send inconsistently. With the header present, the transport
	// honors it instead.
	InitialDelay = 1 * time.Second

	// MaxDelay caps the exponential backoff. The ceiling matches the
	// Retry-After value pkg.go.dev returns when it does send one.
	MaxDelay = 30 * time.Second

	// AttemptTimeout caps a single HTTP attempt. The transport composes this
	// timeout inside the retry policy, so each attempt gets its own deadline
	// and a long Retry-After wait between attempts survives intact.
	AttemptTimeout = 30 * time.Second

	// jitterFactor randomizes each backoff delay so a batch of lookups
	// doesn't retry in lockstep.
	jitterFactor = 0.25
)

// NewTransport wraps inner with a per-attempt timeout plus retry/backoff.
// failsafehttp.NewRetryPolicyBuilder already retries the responses worth a
// second look (a 429 or most 5xx) along with transient network errors, and
// honors a Retry-After header. On top, this adds exponential backoff for the
// no-header case plus a jitter factor, and caps the attempts. Composing the
// timeout policy inside the retry policy bounds each attempt rather than the
// whole sequence. Callers pass the durations, so tests drive the policy with
// their own timing.
//
// ReturnLastFailure keeps retries transparent: once the policy runs out of
// attempts, the transport returns the final response (or error) directly
// rather than a retrypolicy.ExceededError wrapper, so a caller's status
// switch still maps a persistent 429 or 5xx onto its own unexpected-status
// error.
func NewTransport(
	inner http.RoundTripper,
	initialDelay, maxDelay, attemptTimeout time.Duration,
) http.RoundTripper {
	//nolint:bodyclose // failsafe policies use *http.Response generics that confuse bodyclose; nothing to close.
	retryPolicy := failsafehttp.NewRetryPolicyBuilder().
		WithBackoff(initialDelay, maxDelay).
		WithJitterFactor(jitterFactor).
		WithMaxRetries(MaxRetries).
		ReturnLastFailure().
		Build()
	//nolint:bodyclose // same generics issue as the retry policy; nothing to close.
	attemptDeadline := timeout.New[*http.Response](attemptTimeout)
	return failsafehttp.NewRoundTripper(inner, retryPolicy, attemptDeadline)
}
