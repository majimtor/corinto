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
# The seed is asked for no history: the figures below are read from the
# whole database, and are of what this run serves.
key=$(dc exec --no-TTY api /corinto seed --no-history 2>/dev/null | grep -o '"key":"cor_[^"]*"' | head -n 1 | cut -d '"' -f 4)
[ -n "$key" ] || { echo "the seed printed no key" >&2; exit 1; }

failed=0
dc --profile load run --rm \
	--env CORINTO_KEY="$key" --env MODE --env RATE --env DURATION --env ADDRESSES --env SWITCHES --env SWITCH_EVERY \
	--env SERVICE_P99_MS --env DEACTIVATION_P99_MS --env BREAK_STEP --env BREAK_EVERY --env BREAK_CEILING \
	k6 run /bench/redirect.js || failed=1
# The break test ends when a window misses the objective: that is its
# result, said by the last window it printed, and not a failure of the run.
[ "$mode" != break ] || failed=0

# What the load generator cannot see is read from the database, once it has
# stopped changing. `redirect` writes what it holds every second, the
# counts of what it served first and the events after them, and the worker
# counts an event within another second. So that nothing is pending is not
# enough: it is also true before the last of those writes arrives. Two
# readings three seconds apart that are the same, with nothing pending,
# have no write between them.
state() {
	sql "SELECT concat_ws(' ', (SELECT coalesce(sum(value), 0) FROM pipeline_counters), (SELECT count(*) FROM event_inbox),
	                      (SELECT count(*) FROM event_inbox WHERE processed_at IS NULL))"
}
waited=0
before=
while now=$(state) && { [ "$now" != "$before" ] || [ "${now##* }" != 0 ]; }; do
	[ "$waited" -lt 120 ] || { echo "after two minutes the counts still change or events are still pending" >&2; failed=1; break; }
	waited=$((waited + 3))
	before=$now
	sleep 3
done

# The figures and whether they meet the objectives are read by one
# statement, which sees one moment: what is printed is what was judged.
# Lost is what was served and never accepted. The lag of an event is from
# its access, which is kept to the second, to the moment it was counted.
# The storage is what an event takes in the inbox, with its indexes and
# with the rows that processing it left behind, at the volume of the plan:
# a million events a day, kept for thirty days.
figures=$(sql "
WITH counted AS (
    SELECT coalesce(sum(value) FILTER (WHERE name = 'served'), 0) AS served,
           coalesce(sum(value) FILTER (WHERE name = 'accepted'), 0) AS accepted
      FROM pipeline_counters
), inbox AS (
    SELECT count(*) AS events,
           coalesce(percentile_cont(0.95) WITHIN GROUP (ORDER BY extract(epoch FROM processed_at - occurred_at))
                        FILTER (WHERE processed_at IS NOT NULL), 0) AS lag,
           pg_total_relation_size('event_inbox')::numeric / greatest(count(*), 1) AS bytes
      FROM event_inbox
)
SELECT inbox.events, counted.served - counted.accepted, round(inbox.lag::numeric, 1), trunc(inbox.bytes),
       round(inbox.bytes * 30000000 / 1000000000, 1),
       counted.served = counted.accepted AND inbox.lag < $lag_p95_seconds
           AND inbox.bytes * 30000000 < $storage_gb * 1000000000.0
  FROM counted, inbox")
IFS='|' read -r accepted lost lag bytes gigabytes met <<FIGURES
$figures
FIGURES
echo "figure: events accepted $accepted"
echo "figure: events lost $lost"
echo "figure: ingestion lag p95 $lag s"
echo "figure: storage of an event $bytes bytes, $gigabytes GB at the design volume"
if [ "$mode" != break ] && [ "$met" != t ]; then
	echo "an objective read from the database was missed: no loss, a lag under $lag_p95_seconds s at the 95th percentile, storage under $storage_gb GB" >&2
	failed=1
fi

[ "$failed" = 0 ] || { echo "the $mode run missed an objective" >&2; exit 1; }
[ "$mode" != break ] || { echo "the break run ended: the windows above say how far the rate went and what the service time was"; exit 0; }
echo "the $mode run met its objectives"
