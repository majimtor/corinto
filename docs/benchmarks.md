# Published figures

The objectives of PLAN.md section 15 that a load run can check, measured.
Every figure is the median of five runs of one command, with the smallest
and the largest, and each run is the whole scenario: ten minutes. How a
figure is measured is in PLAN.md section 15 and in ADR 0020; this document
holds what was measured, where and when. What was not measured is at the
end, as an objective and not as a fact. The memory of the replica, which
the plan says is published (section 3), has a section of its own, with
its conditions.

## Conditions

| | |
| --- | --- |
| Date | 2026-11-20 |
| Machine | MacBook Pro (MacBookPro16,2): Intel Core i5-1038NG7, 4 cores and 8 threads at 2.0 GHz, 16 GB of memory, macOS 26.1 |
| Containers | Docker Desktop 4.100.0, engine 29.9.3, with 8 CPUs and 8 GB for all the containers together |
| Limits | `redirect`: 1 CPU and 256 MB. PostgreSQL 18.7: 1 CPU and 1 GB. `api`, `worker` and the load generator: none |
| Load | k6 2.4.0, at a constant arrival rate: 300 requests a second for 10 minutes, from 1,000 simulated client addresses, with the rate limit of the redirect in the path |
| Other load | None that anybody started. A browser with idle tabs and a terminal were open, and the load generator ran on the same machine as the system |
| Commands | `make bench`, five times, one after another; then `make bench-break`, once |
| Commit | `bb16576` |

The figures are of this machine. They say how the design behaves under
the limits above, and little about any other machine.

## The burst

All five runs met every objective. In each, the generator sent every one
of its 180,001 requests, and every answer was the one the contract of the
redirect gives.

| Objective | Median | Smallest | Largest |
| --- | --- | --- | --- |
| Service time of the redirect under 10 ms at the 99th percentile | up to 0.25 ms | up to 0.25 ms | up to 0.25 ms |
| No counted loss | 0 events | 0 | 0 |
| Ingestion lag under 30 s at the 95th percentile | 1.9 s | 1.3 s | 2.4 s |
| Deactivation within 5 s at the 99th percentile | 2.02 s | 1.92 s | 2.03 s |
| Raw events under 10 GB at the design volume | 9.5 GB | 9.4 GB | 9.7 GB |

**Service time.** It is the time the `redirect` process measures of
itself, from the moment it takes a request to the moment it has nothing
left to do for it, with no network on either side. It comes from a
histogram, so each figure is the end of the bucket the percentile falls
in, not a value that was observed: "up to 0.25 ms" is more than 0.1 ms and
at most 0.25 ms. The five runs fell in the same buckets.

| Percentile | Service time, in all five runs |
| --- | --- |
| 50th | up to 0.05 ms |
| 95th | up to 0.1 ms |
| 99th | up to 0.25 ms |
| 99.9th | up to 0.5 ms |

No request took longer than the last bucket holds, one second. Each run
measured about 183,100 requests: the 180,001 of the burst and the ones
that watched a link being disabled.

**Loss.** Each run served between 181,234 and 181,403 responses that owed
an event, and every event reached the inbox.

**Ingestion lag.** From the access of an event, which is kept to the
second, to the moment the worker had counted it. Being kept to the second
adds up to a second to what is measured.

**Deactivation.** A link was disabled a hundred times in each run, while
the burst went on. The time is from the answer of the API to the first
`404` of the redirect, which asks for changes every two seconds: the
median of the five medians was 0.81 s, and the largest time of the five
hundred was 2.03 s.

**Storage.** What an event took in the inbox, with its indexes, when a run
ended: 316 bytes at the median, between 314 and 323. The figure of the
table is that size for thirty million events, a million a day kept for
thirty days. It is the objective with the least room. It also depends on
when it is read: an event is written once and marked as processed once,
the second write leaves the first behind, and the space comes back when
PostgreSQL has cleaned the table. Twenty seconds after a start, before any
cleaning, a short run measured 490 bytes an event.

### What the load generator measured

The time of each request as k6 saw it has the network between the
containers and the generator itself in it. It is not what the objective
is about, and it is here so that the two can be told apart.

| Percentile | Median | Smallest | Largest |
| --- | --- | --- | --- |
| 50th | 0.73 ms | 0.70 ms | 0.76 ms |
| 95th | 1.00 ms | 0.99 ms | 1.10 ms |
| 99th | 1.27 ms | 1.23 ms | 1.47 ms |
| 99.9th | 5.32 ms | 4.82 ms | 11.24 ms |
| Slowest request | 92.8 ms | 82.5 ms | 280.8 ms |

## The break test

The plan asks for the rate to be raised until the first objective fails,
and for where and why. On this machine none failed before the load
generator did.

`make bench-break` raised the rate by 300 requests a second every thirty
seconds, from 300 to 6,000, and read the service time of the server every
ten seconds. In all sixty windows the 99th percentile stayed at up to
0.25 ms, but for two that were up to 0.5 ms, at 5,700 and at 6,000
requests a second. Over the whole run the server answered 1,886,000
requests, with the 99.9th percentile up to 2.5 ms; 1,867,173 events were
accepted and none was lost; and the ingestion lag was 2.3 s at the 95th
percentile.

What gave way first was the generator. Over the run it left out 1,142 of
its 1.9 million requests, and one window of the last step measured 5,930
requests a second instead of 6,000. It shares four cores with the system
it loads. So the figure is a floor: the first objective fails somewhere
above twenty times the burst, and this machine cannot say where.

## The memory of the replica

`redirect` answers from a copy of every link, held in memory (ADR 0006),
so its memory grows with the number of links. Two things were measured at
four sizes, both over what the process held before it had any link: what
the copy holds once it is loaded, and the most the process holds from the
system while the whole copy is read again, which is every five minutes.
The second is the account of the Go runtime: what it has asked the system
for, less what it has given back. At that moment the rows that were just
read and the copy that is being built are alive next to the copy that is
still answering, and that is the moment a memory limit has to fit.

Memory is in MiB here, 1,048,576 bytes, because the limit is counted in
them: the 256 MB the plan gives `redirect` are 256 MiB for Docker.

| | |
| --- | --- |
| Date | 2026-11-21 |
| Machine | The one of the conditions above |
| Process | A test process of Go 1.27.4 on macOS, with the collector at its defaults, as `redirect` runs it. No container and no limit |
| Links | Each with an identifier of 8 characters and a destination of 113, campaign parameters included |
| Other load | The machine was not reserved: a browser and a terminal were open. These are figures of memory, which that does not move |
| Command | `make bench-replica`, which measures five times |
| Commit | `v0.1.0`, with the benchmark that came with these figures |

| Links | Held by the copy | Per link | Most held while it is read again: median | Smallest | Largest |
| --- | --- | --- | --- | --- | --- |
| 100,000 | 37.9 MiB | 397 bytes | 88.2 MiB | 87.0 MiB | 89.6 MiB |
| 200,000 | 75.8 MiB | 397 bytes | 175.9 MiB | 173.8 MiB | 177.0 MiB |
| 250,000 | 117.2 MiB | 492 bytes | 264.7 MiB | 264.6 MiB | 265.9 MiB |
| 1,000,000 | 469.0 MiB | 492 bytes | 1,059 MiB | 1,059 MiB | 1,071 MiB |

What the copy holds came out the same in the five runs, within 0.02 MiB.
A link does not cost one figure: the map behind the copy grows in steps,
and a link costs more when the map has just grown, as between 200,000 and
250,000.

The plan expects 100,000 links (PLAN.md section 15), a number it set
after this measurement. With them the process holds 88 MiB of its 256
while the copy is read again: the objective is met, and the command fails
when it is not. By these figures a copy of 200,000 links still stays
80 MiB under the limit, and one of 250,000 goes 9 MiB over it. The
condition that reopens ADR 0006, the memory of the replica over the limit
of the process at the expected number of links, would be met somewhere
between those two sizes.

The sizing probe of that record had 34 MB for 100,000 links and 265 MB for
a million. The copy as it is built holds 39.7 MB and 491.7 MB, which are
the 37.9 and 469.0 MiB above.

## What was not measured

- **Panel queries over the seed under 200 ms at the 95th percentile.**
  The panel and the synthetic history of the seed belong to the next
  slice. It will be measured there, over that history.
- **Where the system breaks.** Not below 6,000 requests a second, as
  above. Finding it needs a load generator on another machine.
- **Anything with a network.** The service time has none in it, and no
  figure here includes TLS or a connection from outside the machine.
- **The design volume as such.** One million events a day is 12 a second
  on average, a twenty-fifth of the burst that was measured.
- **`redirect` itself with that many links under its limit.** The memory
  of the replica is of the copy alone, in a test process. The process has
  more to hold: the events that wait, the user-agents it has classified,
  the keys of its rate limit.

## Reproducing them

```sh
make bench          # the burst, ten minutes; five runs make a figure
make bench-break    # the break test, ten minutes
make bench-replica  # the memory of the replica, five times in half a minute
```

The first two need Docker and `make`. Each run starts a stack of its own,
seeds it, and removes it with its data when it ends. A run fails when an
objective is missed, and prints each figure on a line that begins with
`figure:`. The third needs Go and `make`, and about 1.2 GB of memory for
its largest size. It prints a line for each size and run: `held-MiB` and
`held-B/link` are what the copy holds, and `most-MiB` the most the process
held. It fails when the links the plan expects do not fit in the limit of
`redirect`.
