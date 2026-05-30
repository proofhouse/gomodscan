#!/usr/bin/env bash
# Strip git's commit-message helper text and any verbose-diff
# scissors block from the commit-message buffer, then lint the result
# under vale's commit-message scope. The commit-msg hook fires before
# git's own cleanup pass, so the buffer still contains:
#
#   - Lines beginning with the comment character (`#` by default).
#     git adds these on `git commit --amend` or `git commit` without
#     `-F`/`-m` to remind the author what changed and what gets
#     ignored. The text reads like English prose, so vale flags it
#     for headings, "will", passive voice, and so on.
#
#   - Everything from the scissors line
#     `# ------------------------ >8 ------------------------` to
#     EOF when `commit.verbose=true` is set, which is the project
#     default for any reviewer who wants the diff visible while
#     editing. The diff itself contains `--git` (em-dash trigger)
#     and other tokens vale would otherwise treat as prose.
#
# The hook receives the message-buffer path as `$1` (`.git/COMMIT_EDITMSG`
# from git, or `COMMIT_AGENTMSG` from the `lint-commit-msg` recipe).
set -euo pipefail

msg_file=$1

# vale selects its rule scope from the path of the file it lints. An
# earlier version copied the cleaned message to a temp `*.md` file,
# which matched the generic `[*.md]` scope and silently skipped the
# `ai-tells-commits` rules that only the `[{COMMIT_EDITMSG,...}]` scope
# enables. Piping the cleaned text on stdin and labelling it via
# `--path` with the message basename drives both format resolution
# (`[formats]` maps the extensionless name to markdown) and scope
# selection, so the commit-message tells now fire. The basename keeps
# the match working whether `$1` arrives relative or absolute.
#
# sed cuts from the scissors marker through EOF. The pattern matches
# the scissors text loosely (any number of dashes, optional spacing
# around the `>8`) and accepts the configurable `core.commentChar`
# via the `[#;]` character class. `git stripspace --strip-comments`
# then drops the remaining comment lines using git's own commentChar
# rules.
sed -E '/^[#;] *-+ *>8 *-+ *$/,$d' "$msg_file" \
  | git stripspace --strip-comments \
  | vale --path="$(basename "$msg_file")"
