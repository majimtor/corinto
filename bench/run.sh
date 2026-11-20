#!/bin/sh
# Runs a load scenario against a stack of its own and says whether the
# objectives of PLAN.md section 15 were met. Usage, from the root of the
# repository, through make:
#
#   bench/run.sh burst   the burst of the plan: what the figures are of
#   bench/run.sh smoke   a few seconds with wide thresholds, as CI runs it
#   bench/run.sh break   the rate raised until the service time misses
#
# The stack is deploy/compose.bench.yaml: the processes of the local stack
# with the limits of the plan, under another project name. It is removed
# with its data when the run ends, however it ends.
set -eu

mode=${1:-burst}
# The objectives. A smoke run holds the system to ten times each: it says
# that the command works and that nothing got worse by an order of
# magnitude, and no more than that.
lag_p95_seconds=30
storage_gb=10
case $mode in
burst) ;;
smoke)
	export RATE=50 DURATION=20s SWITCHES=3 SERVICE_P99_MS=100 DEACTIVATION_P99_MS=50000
	lag_p95_seconds=300
	storage_gb=100
	;;
break) export MODE=break ;;
*)
	echo "usage: $0 burst|smoke|break" >&2
	exit 2
	;;
esac

# The image of the load generator is named by the Makefile, where the
# versions of the tools are pinned.
: "${K6_IMAGE:?run it with make bench, which names the image of the load generator}"
export K6_IMAGE

dc() { docker compose --file deploy/compose.bench.yaml "$@"; }
sql() { dc exec --no-TTY db psql --username corinto --dbname corinto --no-align --tuples-only --command "$1"; }

trap 'dc --profile load down --volumes >/dev/null 2>&1' EXIT
dc up --build --detach --wait db migrate api redirect worker

# The seed prints a key of each demo workspace, once. The first is handed
# to the load generator, which creates a link with it, and is shown nowhere.
key=$(dc exec --no-TTY api /corinto seed 2>/dev/null | grep -o '"key":"cor_[^"]*"' | head -n 1 | cut -d '"' -f 4)
[ -n "$key" ] || { echo "the seed printed no key" >&2; exit 1; }

failed=0
dc --profile load run --rm \
	--env CORINTO_KEY="$key" --env MODE --env RATE --env DURATION --env ADDRESSES --env SWITCHES --env SWITCH_EVERY \
	--env SERVICE_P99_MS --env DEACTIVATION_P99_MS --env BREAK_STEP --env BREAK_EVERY --env BREAK_CEILING \
	k6 run /bench/redirect.js || failed=1
# The break test ends when a window misses the objective: that is its
# result, said by the last window it printed, and not a failure of the run.
[ "$mode" != break ] || failed=0

# What the load generator cannot see is read from the database, once the
# worker has gone through what was accepted.
waited=0
while [ "$(sql "SELECT count(*) FROM event_inbox WHERE processed_at IS NULL")" != 0 ]; do
	waited=$((waited + 1))
	[ "$waited" -le 120 ] || { echo "after two minutes events are still pending" >&2; failed=1; break; }
	sleep 1
done

# Lost is what was served and never accepted. The lag of an event is from
# its access, which is kept to the second, to the moment it was counted.
# The storage is what an event takes in the inbox, with its indexes and
# with the rows that processing it left behind, at the volume of the plan:
# a million events a day, kept for thirty days.
echo "figure: events accepted $(sql "SELECT count(*) FROM event_inbox")"
echo "figure: events lost $(sql "SELECT coalesce(sum(value) FILTER (WHERE name = 'served'), 0) - coalesce(sum(value) FILTER (WHERE name = 'accepted'), 0) FROM pipeline_counters")"
echo "figure: ingestion lag p95 $(sql "SELECT round(coalesce(percentile_cont(0.95) WITHIN GROUP (ORDER BY extract(epoch FROM processed_at - occurred_at)), 0)::numeric, 1) FROM event_inbox WHERE processed_at IS NOT NULL") s"
echo "figure: storage of an event $(sql "SELECT pg_total_relation_size('event_inbox') / greatest(count(*), 1) FROM event_inbox") bytes, $(sql "SELECT round(pg_total_relation_size('event_inbox')::numeric / greatest(count(*), 1) * 30000000 / 1000000000, 1) FROM event_inbox") GB at the design volume"
if [ "$mode" != break ]; then
	met=$(sql "SELECT (SELECT coalesce(sum(value) FILTER (WHERE name = 'served'), 0) = coalesce(sum(value) FILTER (WHERE name = 'accepted'), 0) FROM pipeline_counters)
	              AND (SELECT coalesce(percentile_cont(0.95) WITHIN GROUP (ORDER BY extract(epoch FROM processed_at - occurred_at)), 0) < $lag_p95_seconds FROM event_inbox WHERE processed_at IS NOT NULL)
	              AND (SELECT pg_total_relation_size('event_inbox')::numeric / greatest(count(*), 1) * 30000000 < $storage_gb * 1000000000.0 FROM event_inbox)")
	[ "$met" = t ] || { echo "an objective read from the database was missed: no loss, a lag under $lag_p95_seconds s at the 95th percentile, storage under $storage_gb GB" >&2; failed=1; }
fi

[ "$failed" = 0 ] || { echo "the $mode run missed an objective" >&2; exit 1; }
[ "$mode" != break ] || { echo "the break run ended: the windows above say how far the rate went and what the service time was"; exit 0; }
echo "the $mode run met its objectives"
