# sekiban

A StarRocks dashboard that stays up when the cluster does not.

`sekiban` is a fork of [keikiban](https://github.com/crgimenes/keikiban), a
PostgreSQL dashboard, with the collector layer rewritten for StarRocks. Same
shape: a native desktop window, no server, no port, no agent on the cluster.

## The name

`sekiban` (石板) is a flagstone: a flat slab of stone. It is keikiban's
sibling. 板 is a board in both, and what is written on the board changed from
a PostgreSQL notice board to a StarRocks one.

The program was called `sr-dash` until the rename. Nothing else about it
changed, and a config file written under the old name is still read, so the
rename cost nobody a connection.

## What it shows

**CPU time per second, stacked by slice.** StarRocks has no wait events, so
there is nothing to split a session count by; the CPU a query burns is the
thing that actually saturates an MPP engine. The chart can be sliced by
query, user, database, resource group, warehouse, exec state, FE host or
catalog. Sampled once a second from `SHOW PROC '/current_queries'`.

**Top queries**, ranked by the CPU they burned in the window.

**Seven counter panels** under the load chart, each on its own axis so one
scale means one thing:

| Panel | What it shows | Source |
| --- | --- | --- |
| Connections | query / idle / other | `SHOW PROCESSLIST` |
| Queries per second | accepted and failed | `fe_metrics` |
| Query queue | pending queries, running slots | `fe_metrics` |
| Memory in use | busiest backend, and the queries' share | `/backends`, `/current_queries` |
| Bytes per second | disk reads, and what the queries scan | `be_metrics`, `/current_queries` |
| Compaction | bytes compacted per second | `be_metrics` |
| Transactions | in flight, and the completion rate | `/transactions/<db>` |

**Sessions**, with the statement each one is running, and a `KILL` that shows
the exact statement before it runs. StarRocks has one way to stop a session
and not another: `KILL` ends the connection, and the query on it goes with
it.

The metric views cost about 26ms each, so they are read every ten seconds
and the values are carried in between.

**Browser**, the objects of the connected database, and their catalog:
the table model, the primary key, the partition column, the distribution,
the sort key and the properties. Double-click an object to open it in its
own window, where its definition and a console to read and edit it live.

**Compactions & tablet health** — the merges in flight, the tablet work
waiting on each backend, the transactions in flight, and the tablets that
are unhealthy, inconsistent, cloning or in error.

**Query profile** — the frontend's own profile list, an `EXPLAIN ANALYZE`
box, and the **top queries by the CPU they cost**, taken from the frontend's
audit log. The audit log is the only complete record of the queries that
have finished: it is a file on the frontend's own disk, so it is read over
the frontend's HTTP API when you ask for it, and only then.

**Loads** — the import history, the routine loads and the stream loads.

**Resources & limits** — the warehouses and the compute they hold, the
resource groups and the limits that apply to a workload, and the cluster's
own configuration.

**Metrics explorer** — every metric the frontend and the backends publish
about themselves: the SQL projection of their Prometheus endpoints, 2978
rows on a four-backend cluster.

**Query editor**, in that object window. Only a primary key table accepts
an update, so only a primary key table's grid can be edited; on every other
kind the grid is read-only and says why. The exact UPDATE is shown before
it runs.

## What it does not show, and why

keikiban's Indexes, Locks, Migrations and Maintenance screens have no
StarRocks analog at all, so they are not here:

- **Indexes** — StarRocks has no per-index scan counters.
- **Locks / blocking tree** — no row locks and no blocking; StarRocks uses
  MVCC plus optimistic concurrency, and queries *queue* rather than block.
- **Migrations** — no DDL versioning.
- **Maintenance** — no XID wraparound, no VACUUM, no sequences. Compaction
  and tablet health are the real equivalents.

## Requirements

StarRocks 4.0 or newer. The `SHOW PROC` paths this program relies on changed
in 4.0; a 3.x server is refused with a message that says so.

Nothing to install on the cluster.

## Build

```
git clone https://github.com/crgimenes/sekiban
cd sekiban
go build -o sekiban .
```

One binary, no dependencies beyond the Go toolchain. It runs on Linux, macOS
and Windows, on amd64 and arm64.

## Configure

Connections live in a [Filo](https://github.com/crgimenes/filo) file at
`~/.config/sekiban/init.filo`, or `./sekiban_init.filo` if you keep one per
project.

```lisp
; sekiban configuration
(database "root:password@tcp(fe.internal:9030)/analytics" "Production")
```

The connection string is a MySQL DSN, the shape the Go driver accepts. The
frontend's HTTP port (8030) is derived from the host; a third argument
overrides it when the FE HTTP port is elsewhere:

```lisp
(database "root:password@tcp(fe.internal:9030)/analytics" "Production" "http://fe.internal:18030")
```

**One connection at a time.** Connecting to one closes the other. The name of
the attached database sits in the sidebar and in the window title, so a
command cannot land on a server you were not looking at.

A config file left under the old name — `~/.config/sr-dash/init.filo` or
`./sr_dash_init.filo` — is still read when there is no `sekiban` one, and is
still the file that gets appended to.

## Command line

The GUI is the product. The flags exist so an AI agent can drive it:

```
sekiban -json connections
sekiban -json sessions
sekiban -json dashboard [seconds]
```

`dashboard` samples for a while before answering, because the chart is built
from samples over time.

## Develop

```
go test ./...                      # the pure parts: the units parser, the aggregation
go test -tags uicheck ./...        # the interface, against a live server
```

The interface can also be opened in a plain browser, with recorded data and
no database behind it:

```
sh tools/uimock/serve.sh
```

The recordings are real captures from a live cluster. Re-record them with
`sh tools/record_demo.sh`; a chart drawn from a synthetic curve would be a
picture of nothing.

## License

MIT, like keikiban.
