// SPDX-License-Identifier: Apache-2.0
// Copyright Authors of Proofhouse

package pkgsite_test

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/proofhouse/gomodscan/internal/pkgsite"
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

// versionsCase drives one TestVersions subtest. Each case supplies a
// handler, optional client overrides, the module path under test, an
// error matcher set, and a verifier for the success path.
type versionsCase struct {
	name       string
	handler    http.HandlerFunc
	clientOpts func(c *pkgsite.Client)
	module     string
	wantErr    []error
	notWantErr []error
	errSubstr  string
	verify     func(t *testing.T, got []pkgsite.ModuleVersion)
}

func TestVersions(t *testing.T) {
	t.Parallel()

	const cobraModule = "github.com/spf13/cobra"
	singlePageBody := `{
		"items": [
			{"modulePath":"` + cobraModule + `","version":"v1.10.2","latestVersion":"v1.10.2","deprecated":false,"retracted":false,"commitTime":"2025-12-03T23:51:15Z"},
			{"modulePath":"` + cobraModule + `","version":"v1.10.1","latestVersion":"v1.10.2","deprecated":false,"retracted":false,"commitTime":"2025-09-01T16:19:51Z"}
		],
		"total": 2
	}`

	const manyVersionsModule = "example.com/many-versions"
	pageBodies := map[string]string{
		"": `{
			"items": [{"modulePath":"` + manyVersionsModule + `","version":"v0.3.0","latestVersion":"v0.3.0"}],
			"total": 3,
			"nextPageToken": "TOKEN-2"
		}`,
		"TOKEN-2": `{
			"items": [{"modulePath":"` + manyVersionsModule + `","version":"v0.2.0","latestVersion":"v0.3.0"}],
			"total": 3,
			"nextPageToken": "TOKEN-3"
		}`,
		"TOKEN-3": `{
			"items": [{"modulePath":"` + manyVersionsModule + `","version":"v0.1.0","latestVersion":"v0.3.0"}],
			"total": 3
		}`,
	}

	const oldAndBustedModule = "example.com/old-and-busted"
	deprecatedRetractedBody := `{
		"items": [
			{"modulePath":"` + oldAndBustedModule + `","version":"v2.0.0","latestVersion":"v2.0.0","deprecated":true,"deprecationReason":"use v3","retracted":false},
			{"modulePath":"` + oldAndBustedModule + `","version":"v1.0.1","latestVersion":"v2.0.0","deprecated":true,"deprecationReason":"use v3","retracted":true,"retractionReason":"breaking checksum"}
		],
		"total": 2
	}`

	const customUA = "depscan-test/0"

	cases := []versionsCase{
		{
			name:   "single page returns all items and headers match contract",
			module: cobraModule,
			handler: func(w http.ResponseWriter, r *http.Request) {
				assert.Equal(t, "/v1beta/versions/"+cobraModule, r.URL.Path)
				assert.Equal(t, "200", r.URL.Query().Get("limit"))
				assert.Empty(t, r.URL.Query().Get("token"))
				assert.Equal(t, pkgsite.DefaultUserAgent, r.Header.Get("User-Agent"))
				w.Header().Set("Content-Type", "application/json")
				mustWrite(t, w, singlePageBody)
			},
			verify: func(t *testing.T, got []pkgsite.ModuleVersion) {
				t.Helper()
				require.Len(t, got, 2)
				assert.Equal(t, "v1.10.2", got[0].Version)
				assert.Equal(t, "v1.10.2", got[0].LatestVersion)
				assert.False(t, got[0].Retracted)
				assert.False(t, got[0].Deprecated)
			},
		},
		{
			name:   "pagination walks every nextPageToken until exhaustion",
			module: manyVersionsModule,
			handler: func(w http.ResponseWriter, r *http.Request) {
				token := r.URL.Query().Get("token")
				body, present := pageBodies[token]
				if !present {
					t.Errorf("unexpected token %q", token)
					return
				}
				w.Header().Set("Content-Type", "application/json")
				mustWrite(t, w, body)
			},
			verify: func(t *testing.T, got []pkgsite.ModuleVersion) {
				t.Helper()
				require.Len(t, got, 3)
				assert.Equal(t,
					[]string{"v0.3.0", "v0.2.0", "v0.1.0"},
					[]string{got[0].Version, got[1].Version, got[2].Version},
				)
			},
		},
		{
			name:   "deprecation and retraction fields decode verbatim",
			module: oldAndBustedModule,
			handler: func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				mustWrite(t, w, deprecatedRetractedBody)
			},
			verify: func(t *testing.T, got []pkgsite.ModuleVersion) {
				t.Helper()
				require.Len(t, got, 2)
				assert.True(t, got[0].Deprecated)
				assert.Equal(t, "use v3", got[0].DeprecationReason)
				assert.True(t, got[1].Retracted)
				assert.Equal(t, "breaking checksum", got[1].RetractionReason)
			},
		},
		{
			name:   "404 wraps ErrNotFound",
			module: "example.com/private/repo",
			handler: func(w http.ResponseWriter, _ *http.Request) {
				http.Error(w, "not found", http.StatusNotFound)
			},
			wantErr: []error{pkgsite.ErrNotFound},
		},
		{
			name:   "5xx wraps ErrUnexpectedStatus",
			module: "example.com/whatever",
			handler: func(w http.ResponseWriter, _ *http.Request) {
				http.Error(w, "boom", http.StatusInternalServerError)
			},
			wantErr:    []error{pkgsite.ErrUnexpectedStatus},
			notWantErr: []error{pkgsite.ErrNotFound},
		},
		{
			name:   "malformed JSON surfaces as a decode error",
			module: "example.com/garbage",
			handler: func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				mustWrite(t, w, "not json at all")
			},
			errSubstr: "decode",
		},
		{
			name:   "custom User-Agent reaches the server unchanged",
			module: "example.com/whatever",
			handler: func(w http.ResponseWriter, r *http.Request) {
				assert.Equal(t, customUA, r.Header.Get("User-Agent"))
				w.Header().Set("Content-Type", "application/json")
				mustWrite(t, w, `{"items":[],"total":0}`)
			},
			clientOpts: func(c *pkgsite.Client) { c.UserAgent = customUA },
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			runVersionsCase(t, tc)
		})
	}
}

// runVersionsCase wires the case's handler into an httptest server
// and points a Client at it. The case's expectations then drive the
// Versions call and the post-call assertions.
func runVersionsCase(t *testing.T, tc versionsCase) {
	t.Helper()
	srv := httptest.NewServer(tc.handler)
	t.Cleanup(srv.Close)

	c := &pkgsite.Client{BaseURL: srv.URL}
	if tc.clientOpts != nil {
		tc.clientOpts(c)
	}

	got, err := c.Versions(t.Context(), tc.module)
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

// TestVersions_ContextCancelled lives outside the table because
// the timing dance (slow handler vs. short deadline) doesn't fit
// the synchronous case shape the table assumes.
func TestVersions_ContextCancelled(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		time.Sleep(200 * time.Millisecond)
		w.Header().Set("Content-Type", "application/json")
		mustWrite(t, w, `{"items":[],"total":0}`)
	}))
	t.Cleanup(srv.Close)

	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Millisecond)
	t.Cleanup(cancel)

	c := &pkgsite.Client{BaseURL: srv.URL}
	_, err := c.Versions(ctx, "example.com/slow")
	require.Error(t, err)
	assert.True(t,
		errors.Is(err, context.DeadlineExceeded) || strings.Contains(err.Error(), "context deadline"),
		"expected deadline-exceeded, got %v", err,
	)
}
