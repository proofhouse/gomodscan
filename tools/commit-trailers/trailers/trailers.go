// SPDX-License-Identifier: Apache-2.0
// Copyright Authors of Proofhouse

// Package trailers enforces project commit-trailer rules that
// commitlint's stock ruleset misses. The rules follow the Linux kernel
// AI coding-assistants policy at
// https://docs.kernel.org/process/coding-assistants.html, with one
// override: the kernel forbids AI assistants from adding their own
// Signed-off-by trailer; this repo allows it because the human
// committer remains responsible for the DCO regardless.
//
// Rule 1: When the commit message carries an Assisted-by trailer,
// the value must match the kernel-style format
// AGENT_NAME:MODEL_VERSION [TOOL1] [TOOL2]. The colon separates
// agent from model. Tool names follow whitespace-separated.
//
// Rule 2: When both Assisted-by and Signed-off-by appear, the
// Assisted-by trailer must come first.
//
// Rule 3: A Co-authored-by trailer attributing the work to a known
// LLM gets rejected. LLM attribution belongs in Assisted-by.
// Human co-authors via Co-authored-by remain valid.
package trailers

import (
	"errors"
	"fmt"
	"regexp"
	"strings"
)

// Sentinel errors. Callers can match each violation through the stdlib
// helper for unwrapping. Lowercase initial words follow the Go
// convention for error strings (staticcheck ST1005); proper-noun
// trailer names appear later in the sentence to keep the lint rule
// happy without losing clarity.
var (
	ErrTrailerOrder             = errors.New("trailer order: Assisted-by must appear before Signed-off-by")
	ErrAssistedByFormat         = errors.New("malformed Assisted-by value: expected AGENT_NAME:MODEL_VERSION [TOOL...]")
	ErrCoAuthoredByForbiddenLLM = errors.New("forbidden Co-authored-by attribution to an LLM; use Assisted-by instead")
)

const (
	assistedByToken = "Assisted-by:"
	//nolint:gosec // G101 false positive: Git trailer key, not a credential.
	signedOffByToken = "Signed-off-by:"
	//nolint:gosec // G101 false positive: Git trailer key, not a credential.
	coAuthoredByToken = "Co-authored-by:"
)

// Compiled regexes live at package scope so the costly Compile call
// runs once per process rather than once per Check invocation.
var (
	// assistedByFormat matches the kernel-style value: agent name
	// (lowercase, dashes, digits), colon, model version, then optional
	// space-separated tool names. Conservative enough to reject the
	// older slash form and trailing punctuation.
	assistedByFormat = regexp.MustCompile(`^[a-z0-9][a-z0-9-]*:[a-z0-9][a-z0-9.-]*( [a-z0-9][a-z0-9.-]*)*$`)

	// llmAuthorPattern marks a Co-authored-by attribution as referring
	// to an LLM. Word-boundary matching avoids false hits on substrings
	// inside ordinary names.
	llmAuthorPattern = regexp.MustCompile(
		`(?i)\b(claude|chatgpt|copilot|gpt|gemini|bard|anthropic|openai|ai|llm)\b`,
	)
)

// Check returns the joined set of any trailer-rule violations found in
// the commit message. Returns nil when the message satisfies every
// rule, including the common case where no trailers appear at all.
func Check(msg string) error {
	var errs []error
	errs = append(errs, checkAssistedByFormat(msg)...)
	errs = append(errs, checkCoAuthoredBy(msg)...)
	if err := checkOrder(msg); err != nil {
		errs = append(errs, err)
	}
	return errors.Join(errs...)
}

func checkAssistedByFormat(msg string) []error {
	var errs []error
	for i, line := range strings.Split(msg, "\n") {
		if !strings.HasPrefix(line, assistedByToken) {
			continue
		}
		value := strings.TrimSpace(strings.TrimPrefix(line, assistedByToken))
		if !assistedByFormat.MatchString(value) {
			errs = append(errs, fmt.Errorf("%w (line %d: %q)", ErrAssistedByFormat, i+1, value))
		}
	}
	return errs
}

func checkCoAuthoredBy(msg string) []error {
	var violations []error
	for n, line := range strings.Split(msg, "\n") {
		if !strings.HasPrefix(line, coAuthoredByToken) {
			continue
		}
		credit := strings.TrimSpace(strings.TrimPrefix(line, coAuthoredByToken))
		if llmAuthorPattern.MatchString(credit) {
			violations = append(violations, fmt.Errorf("%w (line %d: %q)", ErrCoAuthoredByForbiddenLLM, n+1, credit))
		}
	}
	return violations
}

func checkOrder(msg string) error {
	assistedAt, signedAt := scan(msg)
	if assistedAt == -1 || signedAt == -1 {
		return nil
	}
	if assistedAt < signedAt {
		return nil
	}
	return fmt.Errorf("%w (line %d after line %d)", ErrTrailerOrder, assistedAt+1, signedAt+1)
}

// scan walks the commit message and records the zero-based line index
// of the first Assisted-by and Signed-off-by trailers. Returns -1 for
// either when absent. The first return holds the Assisted-by index;
// the second holds the Signed-off-by index.
//
// Using an if/else chain rather than a switch-true form keeps each
// predicate inside a statement block that Go's coverage profile can
// attach a count to, so mutation testing can drive both branches.
func scan(msg string) (int, int) {
	assistedAt, signedAt := -1, -1
	for i, line := range strings.Split(msg, "\n") {
		if assistedAt == -1 && strings.HasPrefix(line, assistedByToken) {
			assistedAt = i
		} else if signedAt == -1 && strings.HasPrefix(line, signedOffByToken) {
			signedAt = i
		}
	}
	return assistedAt, signedAt
}
