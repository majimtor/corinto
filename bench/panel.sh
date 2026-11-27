#!/bin/sh
# Measures the pages of the panel over the history of the seed and says
# whether the objective of PLAN.md section 15 was met: under 200 ms at the
# 95th percentile. Usage, from the root of the repository, through make:
#
#   bench/panel.sh
#
# The stack is deploy/compose.bench.yaml, the one of the load run: the
# processes of the local stack with the limits of the plan, under another
# project name. It is removed with its data when the run ends, however it
# ends.
set -eu

# The image of the load generator is named by the Makefile, where the
# versions of the tools are pinned.
: "${K6_IMAGE:?run it with make bench-panel, which names the image of the load generator}"
export K6_IMAGE

dc() { docker compose --file deploy/compose.bench.yaml "$@"; }

trap 'dc --profile load down --volumes >/dev/null 2>&1' EXIT
dc up --build --detach --wait db migrate api redirect worker

# The seed with its history, which is what the pages are measured over: it
# returns once its ninety days are counted. The keys it prints are shown
# nowhere.
dc exec --no-TTY api /corinto seed >/dev/null 2>&1 || { echo "the seed failed" >&2; exit 1; }

dc --profile load run --rm --env RATE --env DURATION --env PANEL_P95_MS k6 run /bench/panel.js || {
	echo "the panel run missed its objective" >&2
	exit 1
}
echo "the panel run met its objective"
