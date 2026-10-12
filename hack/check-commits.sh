#!/usr/bin/env bash
# Checks the commit messages in a git range against the repository rules:
#   - Conventional Commits subject;
#   - a Signed-off-by trailer (DCO);
#   - at most one Assisted-by trailer, and its value is exactly "LLM";
#   - no model or vendor names, no session URLs.
#
# Usage: hack/check-commits.sh <base>..<head>
set -euo pipefail

range=${1:?usage: $0 <base>..<head>}

types='feat|fix|docs|style|refactor|perf|test|build|ci|chore|revert'
subject_re="^(${types})(\([a-z0-9._/-]+\))?!?: .+"
forbidden_re='claude|anthropic|openai|chatgpt|codex|copilot|gemini|generated with|claude\.ai/|chatgpt\.com/'

failed=0
fail() {
	echo "$1 $2: $3" >&2
	failed=1
}

commits=$(git rev-list --reverse --no-merges "$range")
if [ -z "$commits" ]; then
	echo "no commits in range $range"
	exit 0
fi

for sha in $commits; do
	short=${sha:0:9}
	subject=$(git log -1 --format=%s "$sha")
	message=$(git log -1 --format=%B "$sha")

	if ! grep -Eq "$subject_re" <<<"$subject"; then
		fail "$short" "$subject" "subject is not a Conventional Commit (type(scope): description)"
	fi

	if ! grep -Eq '^Signed-off-by: .+ <.+>$' <<<"$message"; then
		fail "$short" "$subject" "missing Signed-off-by trailer (commit with --signoff)"
	fi

	assisted=$(grep -Ei '^Assisted-by:' <<<"$message" || true)
	if [ -n "$assisted" ]; then
		if [ "$(wc -l <<<"$assisted")" -ne 1 ]; then
			fail "$short" "$subject" "more than one Assisted-by trailer"
		elif ! grep -Eiq '^Assisted-by: LLM$' <<<"$assisted"; then
			fail "$short" "$subject" "Assisted-by must be exactly 'Assisted-by: LLM', got '$assisted'"
		fi
	fi

	if grep -Eiq "$forbidden_re" <<<"$message"; then
		fail "$short" "$subject" "message names a model or vendor, or links a session"
	fi
done

if [ "$failed" -ne 0 ]; then
	exit 1
fi
echo "commit messages in $range are fine"
