#!/usr/bin/env bash
#
# PostToolUse hook (Edit|Write). Formats the edited Go file and vets its
# package, so problems surface at edit time instead of at the pre-commit hook.
# A vet failure exits 2, which feeds the output back to Claude.
set -uo pipefail

file=$(jq -r '.tool_input.file_path // empty')
case "$file" in
*.go) ;;
*) exit 0 ;;
esac
[ -f "$file" ] || exit 0

root=${CLAUDE_PROJECT_DIR:-$(git rev-parse --show-toplevel)}
case "$file" in
"$root"/*) ;;
*) exit 0 ;;
esac

if ! out=$(gofmt -w "$file" 2>&1); then
	echo "gofmt failed for ${file#"$root"/}:" >&2
	echo "$out" >&2
	exit 2
fi

pkg=$(dirname "$file")
if ! out=$(cd "$pkg" && go vet . 2>&1); then
	echo "go vet failed for ./${pkg#"$root"/}:" >&2
	echo "$out" >&2
	exit 2
fi
