#!/bin/sh
# Compares the contract of the API with the one of the last release, and
# fails on a breaking change that contract/CHANGELOG.md does not announce
# (PLAN.md section 7). `make contract` runs it, and names the tool and its
# version.
set -eu

: "${OASDIFF:?run this with make contract, which names the tool}"

cd "$(dirname "$0")/.."

# Without the whole history a release that exists would not be found, and
# the comparison would pass for having nothing to compare with.
if [ "$(git rev-parse --is-shallow-repository)" = true ]; then
	echo "the history is not whole: the last release cannot be told from none" >&2
	exit 1
fi

# The last release this commit descends from. Its tag is vX.Y.Z.
if ! last=$(git describe --tags --abbrev=0 --match 'v[0-9]*' 2>/dev/null); then
	echo "no release yet: there is no released contract to compare with"
	exit 0
fi

released=$(mktemp)
trap 'rm -f "$released"' EXIT
git show "$last:contract/openapi.yaml" > "$released"

# The tool runs through `go run`, which answers 1 whatever the tool
# answered: a comparison that failed cannot be told from a breaking change
# by the answer. So it is asked twice. Without a level to fail on, it fails
# only when it cannot compare, which the changelog cannot excuse.
if ! $OASDIFF breaking "$released" contract/openapi.yaml; then
	echo "the contract could not be compared with the one of $last" >&2
	exit 1
fi
if $OASDIFF breaking "$released" contract/openapi.yaml --fail-on ERR >/dev/null 2>&1; then
	echo "the contract breaks nothing of $last"
	exit 0
fi

# An entry announces a breaking change when it begins with the mark, and it
# counts when it was added after the release.
if git diff "$last" -- contract/CHANGELOG.md | grep -q '^+- \*\*Breaking\.\*\*'; then
	echo "the contract breaks $last, and contract/CHANGELOG.md announces it"
	exit 0
fi
echo "the contract breaks $last, and no entry added to contract/CHANGELOG.md since then begins with **Breaking.**" >&2
exit 1
