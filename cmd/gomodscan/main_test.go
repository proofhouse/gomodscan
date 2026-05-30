// SPDX-License-Identifier: Apache-2.0
// Copyright Authors of Proofhouse

package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/proofhouse/gomodscan/internal/exitcode"
	"github.com/proofhouse/gomodscan/internal/findings"
	"github.com/proofhouse/gomodscan/internal/osv"
	"github.com/proofhouse/gomodscan/internal/pkgsite"
	"github.com/proofhouse/gomodscan/internal/vendormod"
)

// errLookupFailure stands in for any non-not-found pkg.go.dev lookup
// error the fake client surfaces. Declared once so err113 stays quiet.
var errLookupFailure = errors.New("network failure")

// errQueryFailure stands in for any OSV-side error the stub surfaces
// back to the scanner.
var errQueryFailure = errors.New("network failure")

// fakeVersionsClient drives the deprecated scanner from tests by
// returning a canned response (or error) per module path. The interface
// keeps run injectable so the table-driven cases below don't need to
// stand up an httptest server for every branch.
type fakeVersionsClient struct {
	responses map[string][]pkgsite.ModuleVersion
	errors    map[string]error
}

func (f *fakeVersionsClient) Versions(_ context.Context, module string) ([]pkgsite.ModuleVersion, error) {
	if err, ok := f.errors[module]; ok {
		return nil, err
	}
	if vs, ok := f.responses[module]; ok {
		return vs, nil
	}
	return nil, fmt.Errorf("%w: %s", pkgsite.ErrNotFound, module)
}

// fakeVulnsClient drives the malicious scanner from tests. Each entry
// maps "module@version" to a canned response or error.
type fakeVulnsClient struct {
	responses map[string][]osv.Vulnerability
	errors    map[string]error
}

func (f *fakeVulnsClient) Query(_ context.Context, pkg osv.Package, version string) ([]osv.Vulnerability, error) {
	key := pkg.Name + "@" + version
	if err, ok := f.errors[key]; ok {
		return nil, err
	}
	return f.responses[key], nil
}

func TestFindingLevel(t *testing.T) {
	t.Parallel()
	assert.Equal(t, findings.LevelError, finding{kind: kindMalicious}.level())
	assert.Equal(t, findings.LevelWarning, finding{kind: kindRetracted}.level())
	assert.Equal(t, findings.LevelWarning, finding{kind: kindDeprecated}.level())
}

func TestFindingProps(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		f    finding
		want map[string]string
	}{
		{
			name: "retracted with reason",
			f:    finding{kind: kindRetracted, module: "example.com/a", version: "v1.0.0", reason: "checksum"},
			want: map[string]string{"reason": "checksum"},
		},
		{
			name: "retracted without reason",
			f:    finding{kind: kindRetracted, module: "example.com/a", version: "v1.0.0"},
			want: map[string]string{},
		},
		{
			name: "reason whitespace trimmed",
			f:    finding{kind: kindRetracted, module: "example.com/a", version: "v1.0.0", reason: "  checksum  \n"},
			want: map[string]string{"reason": "checksum"},
		},
		{
			name: "deprecated emits latest and reason",
			f: finding{
				kind: kindDeprecated, module: "example.com/b", version: "v0.1.0",
				latest: "v0.2.0", reason: "use v0.2.0",
			},
			want: map[string]string{"latest": "v0.2.0", "reason": "use v0.2.0"},
		},
		{
			name: "malicious id only when summary missing",
			f:    finding{kind: kindMalicious, module: "example.com/a", version: "v1.0.0", id: "MAL-2025-0001"},
			want: map[string]string{"id": "MAL-2025-0001"},
		},
		{
			name: "malicious id plus trimmed summary",
			f: finding{
				kind: kindMalicious, module: "example.com/a", version: "v1.0.0", id: "MAL-2025-0001",
				summary: "  Backdoor introduced  ",
			},
			want: map[string]string{"id": "MAL-2025-0001", "summary": "Backdoor introduced"},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			assert.Equal(t, tc.want, tc.f.props())
		})
	}
}

func TestFindingMessage(t *testing.T) {
	t.Parallel()

	assert.Equal(t,
		"Module retracted at v1.0.0. Reason: checksum.",
		finding{kind: kindRetracted, module: "example.com/a", version: "v1.0.0", reason: "checksum"}.message(),
	)
	assert.Equal(t,
		"Module retracted at v1.0.0. No reason recorded.",
		finding{kind: kindRetracted, module: "example.com/a", version: "v1.0.0"}.message(),
	)
	assert.Equal(t,
		"Module deprecated at latest version v0.2.0. Reason: use v0.2.0.",
		finding{
			kind: kindDeprecated, module: "example.com/b", version: "v0.1.0",
			latest: "v0.2.0", reason: "use v0.2.0",
		}.message(),
	)
	assert.Equal(t,
		"OSV malicious-package advisory MAL-2025-0001.",
		finding{kind: kindMalicious, id: "MAL-2025-0001"}.message(),
	)
	assert.Equal(t,
		"OSV malicious-package advisory MAL-2025-0001: Backdoor introduced.",
		finding{kind: kindMalicious, id: "MAL-2025-0001", summary: "Backdoor introduced"}.message(),
	)
	assert.Equal(t, "Unknown finding.", finding{kind: findingKind("other")}.message())
}

func TestEmitText_UnifiedFormat(t *testing.T) {
	t.Parallel()

	hits := []finding{
		{kind: kindRetracted, module: "example.com/a", version: "v1.0.0", reason: "checksum"},
		{
			kind: kindDeprecated, module: "example.com/b", version: "v0.1.0",
			latest: "v0.2.0", reason: "use v0.2.0",
		},
		{
			kind:    kindMalicious,
			module:  "example.com/c",
			version: "v2.0.0",
			id:      "MAL-2025-0001",
			summary: "Backdoor introduced",
		},
	}

	var buf bytes.Buffer
	require.NoError(t, emitText(&buf, hits))
	assert.Equal(t,
		"warning: gomodscan/retracted: example.com/a@v1.0.0 reason=checksum\n"+
			"warning: gomodscan/deprecated: example.com/b@v0.1.0 latest=v0.2.0 reason=\"use v0.2.0\"\n"+
			"error: gomodscan/malicious-package: example.com/c@v2.0.0 id=MAL-2025-0001 summary=\"Backdoor introduced\"\n",
		buf.String(),
	)
}

func TestEmitSARIF_RegistersAllRulesAndEmitsResults(t *testing.T) {
	t.Parallel()

	hits := []finding{
		{kind: kindRetracted, module: "example.com/a", version: "v1.0.0", line: 10, reason: "checksum"},
		{
			kind: kindDeprecated, module: "example.com/b", version: "v0.1.0", line: 14,
			latest: "v0.2.0", reason: "use v0.2.0",
		},
		{
			kind:    kindMalicious,
			module:  "example.com/c",
			version: "v2.0.0",
			line:    18,
			id:      "MAL-2025-0001",
			summary: "Backdoor",
		},
	}

	var buf bytes.Buffer
	require.NoError(t, emitSARIF(&buf, hits))
	out := buf.String()
	assert.Contains(t, out, `"name": "gomodscan"`)
	assert.Contains(t, out, `"id": "retracted"`)
	assert.Contains(t, out, `"id": "deprecated"`)
	assert.Contains(t, out, `"id": "malicious-package"`)
	assert.Contains(t, out, `"ruleId": "retracted"`)
	assert.Contains(t, out, `"ruleId": "deprecated"`)
	assert.Contains(t, out, `"ruleId": "malicious-package"`)
	assert.Contains(t, out, `"level": "error"`)
	assert.Contains(t, out, `"name": "example.com/a@v1.0.0"`)
	assert.Contains(t, out, `"name": "example.com/c@v2.0.0"`)
	assert.Contains(t, out, `"latest": "v0.2.0"`)
	assert.Contains(t, out, `"id": "MAL-2025-0001"`)
	// Every result carries the vendor manifest as its physical
	// location, with the module's line threaded onto the region so
	// Code Scanning anchors each finding on the real file.
	assert.Contains(t, out, `"uri": "vendor/modules.txt"`)
	assert.Contains(t, out, `"startLine": 10`)
	assert.Contains(t, out, `"startLine": 14`)
	assert.Contains(t, out, `"startLine": 18`)
}

func TestEmitFindings_UnknownFormatErrors(t *testing.T) {
	t.Parallel()
	err := emitFindings(&bytes.Buffer{}, "json", nil)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "unknown -format")
}

func TestCollectDeprecated(t *testing.T) {
	t.Parallel()

	mod := vendormod.Module{Path: "example.com/m", Version: "v1.2.3", Line: 5}

	t.Run("empty version list yields no hits", func(t *testing.T) {
		t.Parallel()
		assert.Nil(t, collectDeprecated(mod, nil))
	})

	t.Run("non-matching versions yield no hits", func(t *testing.T) {
		t.Parallel()
		versions := []pkgsite.ModuleVersion{
			{Version: "v1.2.3", LatestVersion: "v2.0.0", Retracted: false, Deprecated: false},
			{Version: "v2.0.0", LatestVersion: "v2.0.0", Retracted: false, Deprecated: false},
		}
		assert.Nil(t, collectDeprecated(mod, versions))
	})

	t.Run("matching retracted version emits a retracted finding", func(t *testing.T) {
		t.Parallel()
		versions := []pkgsite.ModuleVersion{
			{Version: "v2.0.0", LatestVersion: "v2.0.0", Retracted: false, Deprecated: false},
			{Version: "v1.2.3", LatestVersion: "v2.0.0", Retracted: true, RetractionReason: "checksum"},
		}
		got := collectDeprecated(mod, versions)
		require.Len(t, got, 1)
		assert.Equal(t, kindRetracted, got[0].kind)
		assert.Equal(t, "v1.2.3", got[0].version)
		assert.Equal(t, "checksum", got[0].reason)
	})

	t.Run("retracted at a different version is ignored", func(t *testing.T) {
		t.Parallel()
		versions := []pkgsite.ModuleVersion{
			{Version: "v2.0.0", LatestVersion: "v2.0.0", Retracted: false},
			{Version: "v1.0.0", LatestVersion: "v2.0.0", Retracted: true, RetractionReason: "checksum"},
		}
		assert.Nil(t, collectDeprecated(mod, versions))
	})

	t.Run("latest version deprecated emits a deprecated finding", func(t *testing.T) {
		t.Parallel()
		versions := []pkgsite.ModuleVersion{
			{Version: "v3.0.0", LatestVersion: "v3.0.0", Deprecated: true, DeprecationReason: "use v4"},
			{Version: "v1.2.3", LatestVersion: "v3.0.0", Deprecated: false},
		}
		got := collectDeprecated(mod, versions)
		require.Len(t, got, 1)
		assert.Equal(t, kindDeprecated, got[0].kind)
		assert.Equal(t, "v3.0.0", got[0].latest)
		assert.Equal(t, "use v4", got[0].reason)
	})

	t.Run("deprecation on a non-latest version is ignored", func(t *testing.T) {
		t.Parallel()
		versions := []pkgsite.ModuleVersion{
			{Version: "v3.0.0", LatestVersion: "v3.0.0", Deprecated: false},
			{Version: "v1.2.3", LatestVersion: "v3.0.0", Deprecated: true, DeprecationReason: "use v3"},
		}
		assert.Nil(t, collectDeprecated(mod, versions))
	})

	t.Run("matching retraction and latest deprecation emit both findings", func(t *testing.T) {
		t.Parallel()
		versions := []pkgsite.ModuleVersion{
			{Version: "v2.0.0", LatestVersion: "v2.0.0", Deprecated: true, DeprecationReason: "use v3"},
			{Version: "v1.2.3", LatestVersion: "v2.0.0", Retracted: true, RetractionReason: "checksum"},
		}
		got := collectDeprecated(mod, versions)
		require.Len(t, got, 2)
		// Order tracks the version slice: deprecated entry comes first.
		assert.Equal(t, kindDeprecated, got[0].kind)
		assert.Equal(t, kindRetracted, got[1].kind)
		// Both findings carry the module's manifest line.
		assert.Equal(t, mod.Line, got[0].line)
		assert.Equal(t, mod.Line, got[1].line)
	})
}

func TestCollectMalicious(t *testing.T) {
	t.Parallel()

	mod := vendormod.Module{Path: "example.com/mixed", Version: "v0.5.0", Line: 3}
	cases := []struct {
		name    string
		vulns   []osv.Vulnerability
		wantIDs []string
	}{
		{
			name:    "empty vuln list yields no findings",
			vulns:   nil,
			wantIDs: nil,
		},
		{
			name: "only non-MAL advisories yields no findings",
			vulns: []osv.Vulnerability{
				{ID: "GO-2025-0042", Summary: "Generic vuln"},
				{ID: "GHSA-aaaa-bbbb-cccc"},
				{ID: "CVE-2025-12345"},
			},
			wantIDs: nil,
		},
		{
			name: "MAL-prefixed advisories surface; siblings drop out",
			vulns: []osv.Vulnerability{
				{ID: "GO-2025-0042"},
				{ID: "MAL-2025-0007", Summary: "Backdoor introduced upstream"},
				{ID: "MAL-2025-0008"},
			},
			wantIDs: []string{"MAL-2025-0007", "MAL-2025-0008"},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := collectMalicious(mod, tc.vulns)
			require.Len(t, got, len(tc.wantIDs))
			for i, want := range tc.wantIDs {
				assert.Equal(t, want, got[i].id)
				assert.Equal(t, kindMalicious, got[i].kind)
				assert.Equal(t, mod.Path, got[i].module)
				assert.Equal(t, mod.Version, got[i].version)
				assert.Equal(t, mod.Line, got[i].line)
			}
		})
	}
}

func TestEvaluateDeprecated(t *testing.T) {
	t.Parallel()

	mod := vendormod.Module{Path: "example.com/m", Version: "v1.0.0"}

	t.Run("not-found error is swallowed as a skip", func(t *testing.T) {
		t.Parallel()
		client := &fakeVersionsClient{errors: map[string]error{
			mod.Path: fmt.Errorf("%w: %s", pkgsite.ErrNotFound, mod.Path),
		}}
		got, err := evaluateDeprecated(context.Background(), client, mod)
		require.NoError(t, err)
		assert.Nil(t, got)
	})

	t.Run("other errors propagate wrapped", func(t *testing.T) {
		t.Parallel()
		client := &fakeVersionsClient{errors: map[string]error{mod.Path: errLookupFailure}}
		got, err := evaluateDeprecated(context.Background(), client, mod)
		require.Error(t, err)
		require.ErrorIs(t, err, errLookupFailure)
		assert.Contains(t, err.Error(), "lookup versions")
		assert.Nil(t, got)
	})

	t.Run("happy path forwards to collectDeprecated", func(t *testing.T) {
		t.Parallel()
		client := &fakeVersionsClient{responses: map[string][]pkgsite.ModuleVersion{
			mod.Path: {{Version: "v1.0.0", LatestVersion: "v1.0.0", Retracted: true, RetractionReason: "bad"}},
		}}
		got, err := evaluateDeprecated(context.Background(), client, mod)
		require.NoError(t, err)
		require.Len(t, got, 1)
		assert.Equal(t, kindRetracted, got[0].kind)
	})
}

func TestEvaluateMalicious(t *testing.T) {
	t.Parallel()

	mod := vendormod.Module{Path: "example.com/m", Version: "v1.0.0"}
	key := mod.Path + "@" + mod.Version

	t.Run("network errors propagate wrapped", func(t *testing.T) {
		t.Parallel()
		client := &fakeVulnsClient{errors: map[string]error{key: errQueryFailure}}
		got, err := evaluateMalicious(context.Background(), client, mod)
		require.Error(t, err)
		require.ErrorIs(t, err, errQueryFailure)
		assert.Contains(t, err.Error(), "lookup vulns")
		assert.Nil(t, got)
	})

	t.Run("happy path forwards to collectMalicious", func(t *testing.T) {
		t.Parallel()
		client := &fakeVulnsClient{responses: map[string][]osv.Vulnerability{
			key: {{ID: "MAL-2025-0001", Summary: "Backdoor"}},
		}}
		got, err := evaluateMalicious(context.Background(), client, mod)
		require.NoError(t, err)
		require.Len(t, got, 1)
		assert.Equal(t, "MAL-2025-0001", got[0].id)
	})

	t.Run("empty response yields no findings", func(t *testing.T) {
		t.Parallel()
		client := &fakeVulnsClient{} // any key returns nil, nil from the fake
		got, err := evaluateMalicious(context.Background(), client, mod)
		require.NoError(t, err)
		assert.Nil(t, got)
	})
}

// writeVendorModulesTxt drops a minimal vendor/modules.txt under dir so
// the run-level tests can drive vendormod.Read without spinning up a
// full go module.
func writeVendorModulesTxt(t *testing.T, dir, body string) {
	t.Helper()
	vendorDir := filepath.Join(dir, "vendor")
	require.NoError(t, os.MkdirAll(vendorDir, 0o750))
	require.NoError(t, os.WriteFile(filepath.Join(vendorDir, "modules.txt"), []byte(body), 0o600))
}

func TestRun(t *testing.T) {
	t.Parallel()

	t.Run("returns ToolFailure when modroot lacks vendor/modules.txt", func(t *testing.T) {
		t.Parallel()
		var out, errOut bytes.Buffer
		rc, err := run(
			context.Background(),
			t.TempDir(),
			"text",
			&fakeVersionsClient{},
			&fakeVulnsClient{},
			&out,
			&errOut,
		)
		require.Error(t, err)
		assert.Equal(t, exitcode.ToolFailure, rc)
		assert.Contains(t, err.Error(), "read vendored modules")
	})

	t.Run("returns ToolFailure on an unknown -format", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		writeVendorModulesTxt(t, dir, "# example.com/m v1.0.0\n## explicit\nexample.com/m\n")
		versions := &fakeVersionsClient{responses: map[string][]pkgsite.ModuleVersion{
			"example.com/m": {{Version: "v1.0.0", LatestVersion: "v1.0.0"}},
		}}
		var out, errOut bytes.Buffer
		rc, err := run(context.Background(), dir, "json", versions, &fakeVulnsClient{}, &out, &errOut)
		require.Error(t, err)
		assert.Equal(t, exitcode.ToolFailure, rc)
		assert.Contains(t, err.Error(), "emit findings")
	})

	t.Run("returns OK with the no-findings banner when nothing matches", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		writeVendorModulesTxt(t, dir, "# example.com/clean v1.0.0\n## explicit\nexample.com/clean\n")
		versions := &fakeVersionsClient{responses: map[string][]pkgsite.ModuleVersion{
			"example.com/clean": {{Version: "v1.0.0", LatestVersion: "v1.0.0"}},
		}}
		vulns := &fakeVulnsClient{responses: map[string][]osv.Vulnerability{
			"example.com/clean@v1.0.0": {{ID: "GO-2025-0001"}},
		}}
		var out, errOut bytes.Buffer
		rc, err := run(context.Background(), dir, "text", versions, vulns, &out, &errOut)
		require.NoError(t, err)
		assert.Equal(t, exitcode.OK, rc)
		assert.Empty(t, out.String())
		assert.Contains(t, errOut.String(), "scanned 1 module(s), no findings")
	})

	t.Run("returns Findings with both scanners contributing for one module", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		writeVendorModulesTxt(t, dir, "# example.com/bad v1.0.0\n## explicit\nexample.com/bad\n")
		versions := &fakeVersionsClient{responses: map[string][]pkgsite.ModuleVersion{
			"example.com/bad": {
				{Version: "v1.0.0", LatestVersion: "v1.0.0", Retracted: true, RetractionReason: "checksum"},
			},
		}}
		vulns := &fakeVulnsClient{responses: map[string][]osv.Vulnerability{
			"example.com/bad@v1.0.0": {{ID: "MAL-2025-0001", Summary: "Backdoor"}},
		}}
		var out, errOut bytes.Buffer
		rc, err := run(context.Background(), dir, "text", versions, vulns, &out, &errOut)
		require.NoError(t, err)
		assert.Equal(t, exitcode.Findings, rc)
		assert.Contains(t, out.String(), "warning: gomodscan/retracted: example.com/bad@v1.0.0")
		assert.Contains(t, out.String(), "error: gomodscan/malicious-package: example.com/bad@v1.0.0")
		assert.Contains(t, errOut.String(), "2 finding(s) across 1 module(s)")
	})

	t.Run("logs and skips lookup errors from both scanners", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		writeVendorModulesTxt(t, dir,
			"# example.com/ok v1.0.0\n## explicit\nexample.com/ok\n"+
				"# example.com/explode v2.0.0\n## explicit\nexample.com/explode\n",
		)
		versions := &fakeVersionsClient{
			responses: map[string][]pkgsite.ModuleVersion{
				"example.com/ok": {{Version: "v1.0.0", LatestVersion: "v1.0.0"}},
			},
			errors: map[string]error{"example.com/explode": errLookupFailure},
		}
		vulns := &fakeVulnsClient{
			errors: map[string]error{"example.com/explode@v2.0.0": errQueryFailure},
		}
		var out, errOut bytes.Buffer
		rc, err := run(context.Background(), dir, "text", versions, vulns, &out, &errOut)
		require.NoError(t, err)
		assert.Equal(t, exitcode.OK, rc)
		assert.Contains(t, errOut.String(), "gomodscan: example.com/explode: lookup versions: network failure")
		assert.Contains(t, errOut.String(), "gomodscan: example.com/explode: lookup vulns: network failure")
		assert.Contains(t, errOut.String(), "scanned 2 module(s), no findings")
	})

	t.Run("not-found deprecated lookups stay silent in errOut", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		writeVendorModulesTxt(t, dir, "# example.com/private v1.0.0\n## explicit\nexample.com/private\n")
		// The fakes return ErrNotFound (versions) and an empty result
		// (vulns) for any module path they don't recognize.
		var out, errOut bytes.Buffer
		rc, err := run(context.Background(), dir, "text", &fakeVersionsClient{}, &fakeVulnsClient{}, &out, &errOut)
		require.NoError(t, err)
		assert.Equal(t, exitcode.OK, rc)
		assert.NotContains(t, errOut.String(), "lookup versions")
		assert.Contains(t, errOut.String(), "scanned 1 module(s), no findings")
	})
}

func TestRealMain(t *testing.T) {
	t.Parallel()

	t.Run("unknown flag returns ToolFailure", func(t *testing.T) {
		t.Parallel()
		var out, errOut bytes.Buffer
		rc := realMain([]string{"--nonsense"}, &out, &errOut)
		assert.Equal(t, exitcode.ToolFailure, rc)
	})

	t.Run("missing vendor tree prints the error line and returns ToolFailure", func(t *testing.T) {
		t.Parallel()
		var out, errOut bytes.Buffer
		rc := realMain([]string{"-modroot", t.TempDir()}, &out, &errOut)
		assert.Equal(t, exitcode.ToolFailure, rc)
		// The error-log branch only fires when run returns a non-nil
		// error. Asserting the prefix confirms that branch executed.
		assert.Contains(t, errOut.String(), "gomodscan: read vendored modules:")
	})

	t.Run("version flag prints build metadata and returns OK", func(t *testing.T) {
		t.Parallel()
		var out, errOut bytes.Buffer
		rc := realMain([]string{"-version"}, &out, &errOut)
		assert.Equal(t, exitcode.OK, rc)
		assert.Contains(t, out.String(), "gomodscan ")
		assert.Contains(t, out.String(), "commit:")
		assert.Contains(t, out.String(), "date:")
	})
}
