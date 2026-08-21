# Transfer tuning

easySFTP has three settings that decide how much work it does at once:

| Setting | What it controls |
|---|---|
| `advanced.connections` | How many SSH connections the uploads spread over. |
| `advanced.concurrency` | How many files are transferred in parallel, and how many independent remote scan / delete requests run at once. |
| `advanced.request_concurrency` | How many SFTP write requests **one file** may have in flight (pipelining inside a single transfer). |

All three default to `auto`, and `auto` means easySFTP works the value out for
itself, per deployment, from what it is about to send and from the connection
it just opened. **You do not have to set any of them.** This page explains what
it does, so that when you do want to override something you know what you are
overriding.

> Before v3.6.0, `auto` was a synonym for a fixed `1` / `4` / `16`. It is not
> any more. If you pinned numbers because the fixed defaults were slow for your
> deploy, this is a good moment to try `auto` again.

## The short version

- **Many small files** get a lot of parallel workers and, on a link with real
  latency, a handful of connections.
- **A few large files** get one worker and one connection per file, and deeper
  per-file pipelining.
- **A redeploy that changes almost nothing** gets many workers (the "has this
  changed?" checks are cheap and parallel well) and exactly one connection,
  because a second SSH handshake would cost more than the whole deploy.
- **A server one hop away** gets far less of everything than a server on
  another continent, because the round-trips it is saving are worth less.

## How the choice is made

### 1. What is being deployed

After the local scan, and again for each deployment right before its transfer,
easySFTP knows the files it is about to send: how many, how large in total, the
size of the biggest one, and how many pure metadata round-trips come with them
(the one stat per file that `advanced.skip_unchanged` costs, the removals of a
delete sweep).

`concurrency` follows directly from that: as many workers as there are
independent items, capped at 64. A worker with nothing to do never starts, so
there is nothing to trade off.

`request_concurrency` follows from the largest file. The setting counts 32 KiB
write packets in flight for a single file, so a 4 KiB file cannot use a second
one however high the number is; a 16 MiB file can use many. It stays at the
long-standing 16 for a tree of small files and rises to at most 64 for large
ones, which is where one SSH channel's 2 MiB window is full anyway.

### 2. What the connection costs

The first connection times its own handshake, then measures the round-trip time
with three of the cheapest requests the protocol has (`SSH_FXP_REALPATH` of
`.`, which reads nothing and writes nothing). Both feed the same model.

An extra connection buys a second TCP flow, a second cipher stream and a second
`sftp-server` process on the far side, and costs one full handshake. If a
deploy would take `W` over one connection, `k` connections take roughly
`W/k + (k-1)*H`, which is smallest at `k = sqrt(W/H)`: **open another
connection while the time it saves is larger than the handshake it costs.**
That is the whole rule. It is why a 2000-file deploy over a 13 ms line gets
eight connections and a 1.3-second redeploy over the same line gets one.

The pool is never larger than the number of files being uploaded (only the
per-file upload path spreads over it), never larger than the worker count, and
never larger than 8.

### 3. What the transfer turns out to be like

One thing cannot be known before any bytes move: how fast the link actually is.
easySFTP starts from a deliberately conservative assumption and then measures.
While a transfer runs, it watches the throughput it is really achieving and
widens the connection pool if the work that is left still justifies another
handshake. It grows at most one doubling at a time, and it stops for good the
first time a step does not make things measurably faster.

It also stops immediately if the server refuses a connection or if any upload
has to be retried. A server that is already pushing back is not one to put more
load on.

The pool never shrinks (a handshake already paid is spent, and closing a
connection would abort the files on it), and `concurrency` and
`request_concurrency` never move at runtime: the first is already at the
largest value that can be busy, and the second is fixed when an SFTP client is
created.

This runtime stage is what makes an overlay redeploy safe to start small. With
`advanced.skip_unchanged` easySFTP cannot know in advance how many files really
changed, so it sizes the run for the checks it is certain of. If it then turns
out that most files *are* changing, the ratio it observes in the first second
tells it, and the pool widens.

## Overriding it

Every setting is resolved on its own, so you can pin one and leave the rest
adaptive:

```yaml
advanced:
  connections: 2            # exactly 2, never more, never fewer
  concurrency: auto         # chosen, knowing there are 2 connections
  request_concurrency: 16   # exactly 16
```

A number you write is used verbatim and is never tuned, at any stage. The
policy optimizes inside the constraints your numbers impose: with
`connections: 2` above, the runtime stage is switched off entirely, because
there is nothing it is allowed to change.

Leaving a key out means the same as writing `auto`.

### When to pin something

- **Your server limits concurrent connections.** Shared hosting often does.
  easySFTP degrades on its own (see below), but `connections: 1` avoids the
  warning and the wasted handshake.
- **Your server dislikes parallel writes.** Some SFTP servers behind a quota or
  an antivirus hook serialize badly. `concurrency: 2` is a reasonable probe.
- **You measured something better.** The numbers here are fitted against
  [easySFTP's own benchmarks](../benchmarks/README.md) on one line. If you
  measured a better configuration for your deploy, keep it.

## What you will see in the log

At the default log level a deployment that spreads over more than one
connection says so once:

```text
auto tuning: spreading 2000 file(s) over up to 8 connections, 64 at a time
```

and a runtime change says what moved and on the strength of what measurement:

```text
auto tuning: 1.1 MiB/s over the connections in use; raising connections 2 -> 4 for the rest of this deployment
```

With `log-level: debug` every decision is printed in full, inputs first:

```text
auto tuning: files=2000 bytes=7.8 MiB largest=4.0 KiB probes=0 rtt=13ms handshake=384ms throughput=assumed 1.0 MiB/s -> connections=8 concurrency=64 request_concurrency=16
```

If the server will not open as many connections as easySFTP asked for, you get
one warning and the run continues on what it has:

```text
the server would not open more than 2 SSH connection(s) (...); easySFTP had chosen more and continues with 2
```

That is a degradation, not a failure, and easySFTP stops asking for the rest of
the run. If you see it on every deploy, pin `connections` to the number your
host allows and the warning goes away.

## Limits

The policy is fitted against the sweeps under
[`benchmarks/`](../benchmarks/README.md), which were measured over one line
(around 13 ms round-trip, about 0.36 MiB/s per stream). Two consequences worth
knowing:

- The **hard caps** (8 connections, 64 workers, 64 requests per file) are
  deliberate safety stops, not measured optima. The largest payload in the
  sweeps was still getting faster at 8 connections.
- On a link that is much faster than the one it was fitted on, easySFTP may
  open a connection or two more than it needed to before its own measurement
  corrects the assumption. The cost of that is bounded by the handshakes
  themselves, which on a fast link are cheap.

If your deploy is slower than you expect, `log-level: debug` prints the whole
decision, and [troubleshooting](troubleshooting.md) covers the failure modes
that are not about tuning at all.
