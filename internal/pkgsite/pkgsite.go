// SPDX-License-Identifier: Apache-2.0
// Copyright Authors of Proofhouse

// Package pkgsite provides a minimal client for the public pkg.go.dev
// /v1beta API documented at https://go.dev/blog/pkgsite-api. The
// depscan tool uses it to retrieve per-version deprecation and
// retraction status for each vendored module.
//
// The client wraps GET /v1beta/versions/{module}, decodes the
// [ModuleVersion] records, and walks the nextPageToken chain so the
// caller sees the full version list. Other v1beta endpoints
// (package, search, vulns) stay out of scope.
//
// Field shapes mirror golang/pkgsite's
// cmd/internal/pkgsite-cli/client/types_gen.go, the reference CLI
// from the announcement blog post. The struct duplicates those
// fields rather than importing the internal package, so the API
// contract stays explicit here.
package pkgsite

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"time"

	"github.com/failsafe-go/failsafe-go/failsafehttp"
	"github.com/failsafe-go/failsafe-go/timeout"
)

const (
	// DefaultBaseURL points at the public pkg.go.dev v1beta host.
	// Tests override via [Client.BaseURL].
	DefaultBaseURL = "https://pkg.go.dev"

	// DefaultUserAgent identifies gomodscan traffic to pkg.go.dev. The
	// reference pkgsite-cli client sets a similar self-describing
	// header. A descriptive value lets the upstream operator tell
	// these requests apart from a generic Go HTTP client.
	DefaultUserAgent = "gomodscan/1 (+https://github.com/proofhouse/gomodscan)"

	// defaultPageLimit caps items per page. The API enforces its
	// own ceiling. This value bounds the response size for modules
	// with hundreds of tagged versions.
	defaultPageLimit = 200

	// defaultTimeout caps a single HTTP attempt. The retry transport
	// composes this timeout inside the retry policy, so each attempt
	// gets its own deadline and a long Retry-After wait between attempts
	// survives intact. The value matches the per-request budget the
	// reference pkgsite-cli uses.
	defaultTimeout = 30 * time.Second

	// retryMaxRetries bounds how many times the transport re-issues a
	// request after a retryable response (429 or most 5xx) or a
	// transient network error. Two keeps the worst-case per-module wait
	// modest when pkg.go.dev throttles the whole host.
	retryMaxRetries = 2

	// retryInitialDelay and retryMaxDelay bound the exponential backoff
	// the transport applies after a 429 arrives without a Retry-After
	// header, which pkg.go.dev sends inconsistently. With the header
	// present, the transport honors it instead. The ceiling matches the
	// Retry-After value pkg.go.dev returns when it does send one.
	retryInitialDelay = 1 * time.Second
	retryMaxDelay     = 30 * time.Second

	// retryJitterFactor randomizes each backoff delay so a batch of
	// module lookups doesn't retry in lockstep.
	retryJitterFactor = 0.25
)

// ErrNotFound reports that pkg.go.dev doesn't recognize the module.
// Private modules, modules replaced to a local path, and modules
// never indexed surface as HTTP 404. Callers typically skip them
// rather than treating the result as a failure.
var ErrNotFound = errors.New("module not indexed on pkg.go.dev")

// ErrUnexpectedStatus reports that pkg.go.dev returned an HTTP
// status outside the contract (neither 200 nor 404). The wrapped
// message records the request URL and the status code so the
// caller can decide whether to retry or surface the failure.
var ErrUnexpectedStatus = errors.New("unexpected status from pkg.go.dev")

// defaultHTTPClient backs every [Client] value that doesn't override
// [Client.HTTPClient]. One shared client (and one shared transport)
// lets the underlying connection pool serve every module lookup,
// which avoids a fresh TLS handshake per page.
//
//nolint:gochecknoglobals // intentional process-wide singleton for connection pooling.
var defaultHTTPClient = &http.Client{
	Transport: newRetryTransport(http.DefaultTransport, retryInitialDelay, retryMaxDelay, defaultTimeout),
}

// newRetryTransport wraps inner with a per-attempt timeout plus
// retry/backoff. failsafehttp.NewRetryPolicyBuilder already retries the
// responses worth a second look (a 429 or most 5xx) along with transient
// network errors, and honors a Retry-After header. On top, this adds
// exponential backoff for the no-header case plus a jitter factor, and
// caps the attempts. Composing the timeout policy inside the retry policy
// bounds each attempt rather than the whole sequence. Callers pass the
// durations, so tests drive the policy with their own timing.
//
// ReturnLastFailure keeps retries transparent: once the policy runs out
// of attempts, the transport returns the final response (or error)
// directly rather than a retrypolicy.ExceededError wrapper, so the status
// switch in fetchPage still maps a persistent 429 or 5xx onto
// ErrUnexpectedStatus.
func newRetryTransport(
	inner http.RoundTripper,
	initialDelay, maxDelay, attemptTimeout time.Duration,
) http.RoundTripper {
	//nolint:bodyclose // failsafe policies use *http.Response generics that confuse bodyclose; nothing to close.
	retryPolicy := failsafehttp.NewRetryPolicyBuilder().
		WithBackoff(initialDelay, maxDelay).
		WithJitterFactor(retryJitterFactor).
		WithMaxRetries(retryMaxRetries).
		ReturnLastFailure().
		Build()
	//nolint:bodyclose // same generics issue as the retry policy; nothing to close.
	attemptDeadline := timeout.New[*http.Response](attemptTimeout)
	return failsafehttp.NewRoundTripper(inner, retryPolicy, attemptDeadline)
}

// ModuleVersion mirrors the entry shape returned by
// /v1beta/versions/{module}. Field names match the JSON keys the
// upstream OpenAPI spec documents.
type ModuleVersion struct {
	ModulePath        string    `json:"modulePath"`
	Version           string    `json:"version"`
	CommitTime        time.Time `json:"commitTime"`
	IsRedistributable bool      `json:"isRedistributable"`
	HasGoMod          bool      `json:"hasGoMod"`
	LatestVersion     string    `json:"latestVersion"`
	Deprecated        bool      `json:"deprecated"`
	DeprecationReason string    `json:"deprecationReason"`
	Retracted         bool      `json:"retracted"`
	RetractionReason  string    `json:"retractionReason"`
}

// Client fetches module-version metadata from a pkg.go.dev v1beta
// server. The zero value targets the public host with a 30 s
// timeout. Tests inject an in-memory server through BaseURL.
type Client struct {
	BaseURL    string
	HTTPClient *http.Client
	UserAgent  string
}

func (c *Client) baseURL() string {
	if c.BaseURL != "" {
		return c.BaseURL
	}
	return DefaultBaseURL
}

func (c *Client) httpClient() *http.Client {
	if c.HTTPClient != nil {
		return c.HTTPClient
	}
	return defaultHTTPClient
}

func (c *Client) userAgent() string {
	if c.UserAgent != "" {
		return c.UserAgent
	}
	return DefaultUserAgent
}

// page wraps the paginated response shape the API returns.
type page struct {
	Items         []ModuleVersion `json:"items"`
	Total         int             `json:"total"`
	NextPageToken string          `json:"nextPageToken,omitempty"`
}

// Versions returns every ModuleVersion record pkg.go.dev knows for
// the given module path, walking the nextPageToken chain until the
// API signals exhaustion. Returns ErrNotFound (wrapped) when the
// API responds 404.
func (c *Client) Versions(ctx context.Context, module string) ([]ModuleVersion, error) {
	var all []ModuleVersion
	token := ""
	for {
		p, err := c.fetchPage(ctx, module, token)
		if err != nil {
			return nil, err
		}
		all = append(all, p.Items...)
		if p.NextPageToken == "" {
			return all, nil
		}
		token = p.NextPageToken
	}
}

func (c *Client) fetchPage(ctx context.Context, module, token string) (*page, error) {
	reqURL, err := c.buildURL(module, token)
	if err != nil {
		return nil, err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, reqURL, nil)
	if err != nil {
		return nil, fmt.Errorf("build request for %s: %w", module, err)
	}
	req.Header.Set("User-Agent", c.userAgent())
	req.Header.Set("Accept", "application/json")

	resp, err := c.httpClient().Do(req)
	if err != nil {
		return nil, fmt.Errorf("GET %s: %w", reqURL, err)
	}
	defer resp.Body.Close()

	switch resp.StatusCode {
	case http.StatusOK:
		var p page
		if decErr := json.NewDecoder(resp.Body).Decode(&p); decErr != nil {
			return nil, fmt.Errorf("decode %s: %w", module, decErr)
		}
		return &p, nil
	case http.StatusNotFound:
		return nil, fmt.Errorf("%w: %s", ErrNotFound, module)
	default:
		return nil, fmt.Errorf("%w: GET %s: status %d", ErrUnexpectedStatus, reqURL, resp.StatusCode)
	}
}

func (c *Client) buildURL(module, token string) (string, error) {
	base, err := url.Parse(c.baseURL())
	if err != nil {
		return "", fmt.Errorf("parse base URL %q: %w", c.baseURL(), err)
	}
	u := base.JoinPath("v1beta", "versions", module)
	q := u.Query()
	q.Set("limit", strconv.Itoa(defaultPageLimit))
	if token != "" {
		q.Set("token", token)
	}
	u.RawQuery = q.Encode()
	return u.String(), nil
}
