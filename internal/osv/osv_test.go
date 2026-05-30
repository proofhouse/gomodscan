// SPDX-License-Identifier: Apache-2.0
// Copyright Authors of Proofhouse

package osv_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/proofhouse/gomodscan/internal/osv"
)

// mustWrite writes body to w and records any error against t.
// The httptest handlers run in their own goroutine. t.Errorf stays
// safe from that context: it records the failure without panicking.
func mustWrite(t *testing.T, w http.ResponseWriter, body string) {
	t.Helper()
	if _, err := w.Write([]byte(body)); err != nil {
		t.Errorf("write response body: %v", err)
	}
}

// queryCase drives one TestQuery subtest. Each case supplies a
// handler, optional client overrides, the package and version under
// query, an error matcher set, and a verifier for the success path.
type queryCase struct {
	name       string
	handler    http.HandlerFunc
	clientOpts func(c *osv.Client)
	pkg        osv.Package
	version    string
	wantErr    []error
	notWantErr []error
	errSubstr  string
	verify     func(t *testing.T, got []osv.Vulnerability)
}

func TestQuery(t *testing.T) {
	t.Parallel()

	const cobraModule = "github.com/spf13/cobra"
	emptyBody := `{"vulns":[]}`

	const flaggedModule = "example.com/totally-fine"
	maliciousBody := `{
		"vulns": [
			{
				"id": "MAL-2025-0001",
				"summary": "Malicious code in v1.2.3",
				"aliases": ["GHSA-aaaa-bbbb-cccc"],
				"modified": "2025-01-15T00:00:00Z",
				"published": "2025-01-14T00:00:00Z"
			}
		]
	}`

	const mixedModule = "example.com/has-cve-too"
	mixedBody := `{
		"vulns": [
			{"id": "GO-2025-0042", "summary": "Generic vuln"},
			{"id": "MAL-2025-0007", "summary": "Backdoor introduced upstream"}
		]
	}`

	const customUA = "malscan-test/0"

	cases := []queryCase{
		{
			name:    "empty vulns decodes to a nil-or-empty slice",
			pkg:     osv.Package{Name: cobraModule, Ecosystem: "Go"},
			version: "v1.10.2",
			handler: func(w http.ResponseWriter, r *http.Request) {
				assert.Equal(t, "/v1/query", r.URL.Path)
				assert.Equal(t, http.MethodPost, r.Method)
				assert.Equal(t, "application/json", r.Header.Get("Content-Type"))
				assert.Equal(t, osv.DefaultUserAgent, r.Header.Get("User-Agent"))
				var sent struct {
					Version string `json:"version"`
					Package struct {
						Name      string `json:"name"`
						Ecosystem string `json:"ecosystem"`
					} `json:"package"`
				}
				if decErr := json.NewDecoder(r.Body).Decode(&sent); decErr != nil {
					t.Errorf("decode request body: %v", decErr)
				}
				assert.Equal(t, "v1.10.2", sent.Version)
				assert.Equal(t, cobraModule, sent.Package.Name)
				assert.Equal(t, "Go", sent.Package.Ecosystem)
				w.Header().Set("Content-Type", "application/json")
				mustWrite(t, w, emptyBody)
			},
			verify: func(t *testing.T, got []osv.Vulnerability) {
				t.Helper()
				assert.Empty(t, got)
			},
		},
		{
			name:    "MAL-prefixed advisory decodes verbatim",
			pkg:     osv.Package{Name: flaggedModule, Ecosystem: "Go"},
			version: "v1.2.3",
			handler: func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				mustWrite(t, w, maliciousBody)
			},
			verify: func(t *testing.T, got []osv.Vulnerability) {
				t.Helper()
				require.Len(t, got, 1)
				assert.Equal(t, "MAL-2025-0001", got[0].ID)
				assert.Equal(t, "Malicious code in v1.2.3", got[0].Summary)
				assert.Equal(t, []string{"GHSA-aaaa-bbbb-cccc"}, got[0].Aliases)
			},
		},
		{
			name:    "mixed advisory list preserves order",
			pkg:     osv.Package{Name: mixedModule, Ecosystem: "Go"},
			version: "v0.5.0",
			handler: func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				mustWrite(t, w, mixedBody)
			},
			verify: func(t *testing.T, got []osv.Vulnerability) {
				t.Helper()
				require.Len(t, got, 2)
				assert.Equal(t,
					[]string{"GO-2025-0042", "MAL-2025-0007"},
					[]string{got[0].ID, got[1].ID},
				)
			},
		},
		{
			name:    "non-200 wraps ErrUnexpectedStatus",
			pkg:     osv.Package{Name: "example.com/whatever", Ecosystem: "Go"},
			version: "v0.0.1",
			handler: func(w http.ResponseWriter, _ *http.Request) {
				http.Error(w, "boom", http.StatusInternalServerError)
			},
			wantErr: []error{osv.ErrUnexpectedStatus},
		},
		{
			name:    "malformed JSON surfaces as a decode error",
			pkg:     osv.Package{Name: "example.com/garbage", Ecosystem: "Go"},
			version: "v0.0.1",
			handler: func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				mustWrite(t, w, "not json at all")
			},
			notWantErr: []error{osv.ErrUnexpectedStatus},
			errSubstr:  "decode",
		},
		{
			name:    "custom User-Agent reaches the server unchanged",
			pkg:     osv.Package{Name: "example.com/whatever", Ecosystem: "Go"},
			version: "v0.0.1",
			handler: func(w http.ResponseWriter, r *http.Request) {
				assert.Equal(t, customUA, r.Header.Get("User-Agent"))
				w.Header().Set("Content-Type", "application/json")
				mustWrite(t, w, emptyBody)
			},
			clientOpts: func(c *osv.Client) { c.UserAgent = customUA },
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			runQueryCase(t, tc)
		})
	}
}

// runQueryCase wires the case's handler into an httptest server and
// points a Client at it. The case's expectations then drive the
// Query call and the post-call assertions.
func runQueryCase(t *testing.T, tc queryCase) {
	t.Helper()
	srv := httptest.NewServer(tc.handler)
	t.Cleanup(srv.Close)

	c := &osv.Client{BaseURL: srv.URL}
	if tc.clientOpts != nil {
		tc.clientOpts(c)
	}

	got, err := c.Query(t.Context(), tc.pkg, tc.version)
	hasErrExpectations := len(tc.wantErr) > 0 || len(tc.notWantErr) > 0 || tc.errSubstr != ""
	if !hasErrExpectations {
		require.NoError(t, err)
		if tc.verify != nil {
			tc.verify(t, got)
		}
		return
	}
	require.Error(t, err)
	for _, want := range tc.wantErr {
		require.ErrorIs(t, err, want)
	}
	for _, notWant := range tc.notWantErr {
		require.NotErrorIs(t, err, notWant)
	}
	if tc.errSubstr != "" {
		assert.Contains(t, err.Error(), tc.errSubstr)
	}
}

// TestQuery_ContextCancelled lives outside the table because the
// timing dance (slow handler vs. short deadline) doesn't fit the
// synchronous case shape the table assumes.
func TestQuery_ContextCancelled(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		time.Sleep(200 * time.Millisecond)
		w.Header().Set("Content-Type", "application/json")
		mustWrite(t, w, `{"vulns":[]}`)
	}))
	t.Cleanup(srv.Close)

	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Millisecond)
	t.Cleanup(cancel)

	c := &osv.Client{BaseURL: srv.URL}
	_, err := c.Query(ctx, osv.Package{Name: "example.com/slow", Ecosystem: "Go"}, "v0.0.1")
	require.Error(t, err)
	assert.True(t,
		errors.Is(err, context.DeadlineExceeded) || strings.Contains(err.Error(), "context deadline"),
		"expected deadline-exceeded, got %v", err,
	)
}
