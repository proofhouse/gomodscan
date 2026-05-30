// SPDX-License-Identifier: Apache-2.0
// Copyright Authors of Proofhouse

package trailers_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/proofhouse/gomodscan/tools/commit-trailers/trailers"
)

func TestCheck(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name      string
		msg       string
		wantErrIs []error
	}{
		{
			name: "valid: both trailers in order, kernel format",
			msg: `feat: add thing

Body line.

Assisted-by: claude-code:opus-4.7
Signed-off-by: Tony Burns <tony@tonyburns.net>
`,
		},
		{
			name: "valid: kernel format with optional tool list",
			msg: `feat: add thing

Assisted-by: claude-code:opus-4.7 coccinelle sparse
Signed-off-by: Tony Burns <tony@tonyburns.net>
`,
		},
		{
			name: "valid: only Signed-off-by (hand-written, no LLM)",
			msg: `feat: add thing

Hand-written body.

Signed-off-by: Tony Burns <tony@tonyburns.net>
`,
		},
		{
			name: "valid: human Co-authored-by passes",
			msg: `feat: add thing

Co-authored-by: Alice Example <alice@example.com>
Signed-off-by: Tony Burns <tony@tonyburns.net>
`,
		},
		{
			name: "invalid: order rule fires when Signed-off-by precedes Assisted-by",
			msg: `feat: add thing

Signed-off-by: Tony Burns <tony@tonyburns.net>
Assisted-by: claude-code:opus-4.7
`,
			wantErrIs: []error{trailers.ErrTrailerOrder},
		},
		{
			name: "invalid: slash format (pre-kernel-policy) is rejected",
			msg: `feat: add thing

Assisted-by: claude-code/opus-4.7
Signed-off-by: Tony Burns <tony@tonyburns.net>
`,
			wantErrIs: []error{trailers.ErrAssistedByFormat},
		},
		{
			name: "invalid: Assisted-by missing model version",
			msg: `feat: add thing

Assisted-by: claude-code
Signed-off-by: Tony Burns <tony@tonyburns.net>
`,
			wantErrIs: []error{trailers.ErrAssistedByFormat},
		},
		{
			name: "invalid: Co-authored-by attributing to Claude",
			msg: `fix: tweak something

Co-authored-by: Claude <noreply@anthropic.com>
Signed-off-by: Tony Burns <tony@tonyburns.net>
`,
			wantErrIs: []error{trailers.ErrCoAuthoredByForbiddenLLM},
		},
		{
			name: "invalid: Co-authored-by attributing to GitHub Copilot",
			msg: `docs: update readme

Co-authored-by: GitHub Copilot <copilot@github.com>
Signed-off-by: Tony Burns <tony@tonyburns.net>
`,
			wantErrIs: []error{trailers.ErrCoAuthoredByForbiddenLLM},
		},
		{
			name: "invalid: multiple rule failures combine",
			msg: `feat: add thing

Co-authored-by: ChatGPT <ai@example.com>
Signed-off-by: Tony Burns <tony@tonyburns.net>
Assisted-by: claude-code/opus-4.7
`,
			wantErrIs: []error{
				trailers.ErrCoAuthoredByForbiddenLLM,
				trailers.ErrAssistedByFormat,
				trailers.ErrTrailerOrder,
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			err := trailers.Check(tc.msg)
			if len(tc.wantErrIs) == 0 {
				assert.NoError(t, err)
				return
			}
			require.Error(t, err)
			for _, want := range tc.wantErrIs {
				assert.ErrorIs(t, err, want)
			}
		})
	}
}

// TestCheckErrorMessagesIncludeLineNumbers pins the 1-based line
// numbers that each error message reports. Mutation testing flips
// the `i+1` and `assistedAt+1`/`signedAt+1` arithmetic in the
// formatters; asserting on the literal "line N" text in the error
// surfaces those mutations.
func TestCheckErrorMessagesIncludeLineNumbers(t *testing.T) {
	t.Parallel()

	t.Run("ErrAssistedByFormat reports the trailer's 1-based line", func(t *testing.T) {
		t.Parallel()
		msg := "feat: add thing\n\nAssisted-by: claude-code/opus-4.7\n"
		err := trailers.Check(msg)
		require.Error(t, err)
		// The Assisted-by trailer sits at index 2, so the message
		// reports "line 3" after the 1-based offset.
		assert.Contains(t, err.Error(), `line 3: "claude-code/opus-4.7"`)
	})

	t.Run("ErrCoAuthoredByForbiddenLLM reports the trailer's 1-based line", func(t *testing.T) {
		t.Parallel()
		msg := "fix: x\n\n\nCo-authored-by: Claude <noreply@anthropic.com>\n" +
			"Signed-off-by: Tony Burns <tony@tonyburns.net>\n"
		err := trailers.Check(msg)
		require.Error(t, err)
		// Co-authored-by sits at index 3, so the message reports
		// "line 4" after the 1-based offset.
		assert.Contains(t, err.Error(), `line 4: "Claude <noreply@anthropic.com>"`)
	})

	t.Run("ErrTrailerOrder reports both 1-based line numbers", func(t *testing.T) {
		t.Parallel()
		msg := "feat: add thing\n\nSigned-off-by: Tony Burns <tony@tonyburns.net>\n" +
			"Assisted-by: claude-code:opus-4.7\n"
		err := trailers.Check(msg)
		require.Error(t, err)
		// Signed-off-by sits at index 2 (line 3); Assisted-by at
		// index 3 (line 4). The message reports both in order.
		assert.Contains(t, err.Error(), "line 4 after line 3")
	})
}

// TestCheckOrderRulesOutAdjacentTrailers pins behavior under two
// edge cases: an Assisted-by trailer alone (no Signed-off-by) and
// an out-of-order pair sitting on adjacent lines just after the
// subject. The cases drive the -1 sentinel comparisons in
// checkOrder through inputs that distinguish them from the
// majority-case message shape the rest of the table uses.
func TestCheckOrderRulesOutAdjacentTrailers(t *testing.T) {
	t.Parallel()

	t.Run("Assisted-by alone returns no error", func(t *testing.T) {
		t.Parallel()
		msg := "feat: add thing\n\nAssisted-by: claude-code:opus-4.7\n"
		assert.NoError(t, trailers.Check(msg))
	})

	t.Run("out-of-order trailers on the lines just after the subject", func(t *testing.T) {
		t.Parallel()
		// Signed-off-by on line 2 (index 1), Assisted-by on line 3
		// (index 2). The order rule should fire.
		msg := "feat: add thing\nSigned-off-by: Tony Burns <tony@tonyburns.net>\n" +
			"Assisted-by: claude-code:opus-4.7\n"
		err := trailers.Check(msg)
		require.Error(t, err)
		require.ErrorIs(t, err, trailers.ErrTrailerOrder)
		assert.Contains(t, err.Error(), "line 3 after line 2")
	})

	// Signed-off-by on the subject line (index 0) and Assisted-by on
	// the next line (index 1) drive assistedAt past the 1 boundary
	// the -1 sentinel comparisons would land on under mutation. A
	// commit shaped this way wouldn't ship through prek, but the
	// in-memory message exercises Check directly so the comparison
	// against the 1 mutant produces a different exit than the
	// original -1 check.
	t.Run("subject-line Signed-off-by with Assisted-by next", func(t *testing.T) {
		t.Parallel()
		msg := "Signed-off-by: Tony Burns <tony@tonyburns.net>\n" +
			"Assisted-by: claude-code:opus-4.7\n"
		err := trailers.Check(msg)
		require.Error(t, err)
		require.ErrorIs(t, err, trailers.ErrTrailerOrder)
		assert.Contains(t, err.Error(), "line 2 after line 1")
	})
}
