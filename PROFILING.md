# Profiling

How to measure the engine (`apron/...`), the reference server (`aprond`), and
the client side (`apron/client`, `apron/bot`, `apron/archive`), and how to
tell whether a change made them better.

## Tools

| Command | What it does |
|---|---|
| `make bench` | Runs the Go benchmarks with `-benchmem`, 6 times each, into `build/bench/head.txt`. `BENCH` selects them (a `-bench` pattern), `BENCH_PKGS` the packages, `BENCH_COUNT` the runs, `BENCH_FLAGS` more `go test` flags (such as `-cpu=1,2,4`). |
| `make bench-compare` | Runs the same benchmarks on `BASE` (`origin/main` by default) in a temporary worktree and on the working tree, and compares them with benchstat into `build/bench/compare.txt`. |
| `make profile` | CPU, allocation, mutex, and block profiles of the benchmarks `BENCH` matches in one package `PKG`, into `build/profile`, with the top entries of each. |
| `make bench-smoke` | Each benchmark once (`-benchtime 1x`), as CI runs it, so that benchmarks keep building and running. |
| `cmd/apron-hammer` | Load scenarios against a running server over the network, with server heap and goroutine sampling and profiles through `aprond --debug-addr` ([its README](cmd/apron-hammer/README.md)). |

benchstat is a tool of the root module (`go tool benchstat`), at the version
`go.mod` pins, like staticcheck.

```sh
# One package's fan-out benchmarks, against main.
make bench-compare BENCH=Fanout BENCH_PKGS=./apron/server BENCH_COUNT=10

# How a benchmark scales with CPUs: contention shows as ns/op that does not
# fall, or rises, from -cpu=1 to 4.
make bench BENCH=Parallel BENCH_FLAGS=-cpu=1,2,4

# Where the time, the allocations, and the lock waits go.
make profile PKG=./apron/server BENCH=Fanout/conns=1000
go tool pprof -http=:0 build/profile/cpu.pprof
go tool pprof -list 'server.\(\*Conn\).Send' build/profile/mem.pprof
```

## Measuring

- Compare with benchstat, never single runs: at least 6 runs each
  (`BENCH_COUNT`), 10 for small differences. A result without a confidence
  interval, or marked `~`, is not a difference.
- Timing (`sec/op`) is only as quiet as the machine. Run nothing else
  meanwhile; on a shared or loaded machine, trust `allocs/op` and `B/op`,
  which load does not change, over `sec/op`.
- Allocations are the usual lever: each one costs the allocation itself, and
  its share of GC work later, which a benchmark of one request hides but a
  server under load pays. Read `-sample_index=alloc_space` for the bytes and
  `alloc_objects` for the count.
- Contention shows in mutex and block profiles (`make profile` turns both on
  at full rate) and in how `sec/op` changes with `-cpu`.
- A benchmark measures what it repeats. Set up outside the timed loop
  (`for b.Loop()`), and report per-item costs with `b.ReportMetric` where a
  loop iteration covers many items, such as per recipient of a fan-out.
- `apron-hammer` measures a whole server over real sockets. Its numbers move
  with the machine more than benchmarks do: compare runs on the same machine,
  with the server restarted between them.

## Benchmarks

Each runs alone with `go test -run '^$' -bench '<Name>' -benchmem <package>`
(from `apron/` for the apron module's packages), or through `make bench
BENCH=<Name> BENCH_PKGS=<package>`. Besides the standard columns, many
report their own metrics:

| Metric | What it is |
|---|---|
| `ns/recipient`, `allocs/recipient`, `B/recipient` | A fan-out's cost for each connection it reaches. |
| `send-ns/recipient` | The time the send call itself takes for each recipient: how long it holds the backend's lock, which the engine asks a backend to send under. |
| `lock-ns/op` | How long a request's handler holds the backend's lock (`Reply.Reserve` against `Send`). |
| `writer-ns/post` | A post's latency while the benchmarked requests run: what they cost others through aprond's lock. |
| `frames/op` | Frames that reached their recipients for each operation; divide ns/op by it for a cost per frame. |
| `storeB/op`, `batches/op` | Bytes and batches aprond writes to its store for each operation. |
| `ns/record`, `read-ns/record`, `kept-B/record` | A replay's or an archive's cost for each record, reading one back, and the memory it keeps for each. |
| `retainedB/op` | Heap kept for each message logged. |
| `goroutines/conn`, `B-inuse/conn` | What an idle connection keeps. |

`-short` skips the largest cases, such as a room of 5,000 members, whose
setup takes seconds. CI runs every benchmark once with `-short`
(`make bench-smoke`).

### Engine: `apron/server`

The backend is `apron/internal/benchchat`, shaped like `examples/minimal`'s,
on an in-memory transport.

| Benchmark | What it measures |
|---|---|
| `FanoutBroadcast/conns=10…10000` | One frame to every connection with `Server.Broadcast`: per-recipient cost and send time. |
| `FanoutSend/conns=N/{send,sendall}` | One frame to a backend's own member list, a `Conn.Send` each or one `server.SendAll`. |
| `FanoutNotify` | A notification encoded for each connection, the costly way, for comparison. |
| `FanoutRooms/members={10,1000}/{broadcast,memberlists,status,status-memberlists}` | Rooms among 1,000 connections: a filter against member lists, and `PublishUser` against `PublishUserTo`. |
| `FanoutStorm/{reactions,activity,status}` | Many tiny frames to a room of 100. |
| `FanoutSlowConsumer/stalled=0,1,10` | Stalled connections among fast ones: ns/op must not grow, and the benchmark fails if the stalled ones are not dropped. |
| `RequestRoundTrip/{message,history,history-raw,me,reactions,unsupported}` | One request's whole path on one connection: allocs/op is the request path's cost. |
| `RequestParallel/{message,history,ext:noop,ext:noop/nodedup}` | Requests on many connections at once: run with `-cpu=1,2,4` and a mutex profile. |
| `RequestReserve/{send,reserve,parallel-send,parallel-reserve}` | A large result encoded under the backend's lock or after it (`lock-ns/op`). |
| `RequestLocal` | A `Local.Call`. |
| `RequestLarge` | A 200 KB message: watch B/op. |
| `ConnChurn`, `ConnIdle`, `ConnUser` | A connection's life, what an idle one keeps, and reading its identity. |
| `DedupUsers/users=10,10000` | Deduplication across many users, with eviction. |

### Engine internals, protocol types, and the client side

| Package | Benchmark | What it measures |
|---|---|---|
| `apron/internal/core` | `RequestParse`, `RequestFingerprint`, `DedupCache/size=16,1024` | Parsing a frame, a request's fingerprint, the dedup cache. |
| `apron` | `MessageCodec`, `RoomListEncode`, `ReplayHistory`, `ReplayRaw` | Encoding and decoding messages and room listings; replaying 1M records decoded or raw (`ns/record`, `kept-B/record`). |
| `apron/bot` | `BotEchoStorm` | A bot answering a storm of messages on a Local connection. |
| `apron/client` | `ClientReceive/{message,reply}`, `ClientFrames`, `ClientPost`, `ClientHistoryPages` | The network client's read path, frames over a WebSocket, posting, and paging 100k records. |
| `apron/archive` | `ArchiveWrite`, `ArchiveNext`, `ArchiveRead` | Writing, streaming, and replaying an archive of 1M records (several seconds with setup). |

### Reference server: `aprond`, `store`

Clients are in-memory transports on `srv.ServeTransport`; `store=memory` and
`store=sqlite` cases compare the stores. Some need a fixed count of
iterations, because users have rate limits: use `-benchtime=1000x` for the
fan-outs and storms, `200x` for `RoomListMembers`, and `500x` for
`RoomListUnderWrites`.

| Benchmark | What it measures |
|---|---|
| `RoomFanout/members={10,100,1000,5000}/store=…` | One poster in a room: ns/op and allocs/op against `frames/op`. |
| `RoomFanoutPush/pushers={10,100}` | The same, with members registered for push. |
| `RoomManySmall/store=…` | 32 rooms of 9 posting at once: contention on aprond's lock (`-cpu=1,2,4`). |
| `RoomSlowConsumers` | 1,000 members, a tenth of them stalled: the poster must not slow down. |
| `RoomJoinLeave/members={100,1000}` | A join or leave in a large room (`storeB/op`). |
| `RoomListMembers/rooms=…/members=…`, `RoomListUnderWrites` | `room_list` with members, alone and while others post (`writer-ns/post`). |
| `HistoryPageUnderWrites/store=…` | History pages of 100 and 1,000 over 20,000 messages while two writers post. |
| `ActivityReadStorm/members={100,1000}`, `ActivityTypingStorm` | Read cursors and typing from many members. |
| `StatusFlap/members={100,1000}`, `StatusFlapBystanders/bystanders={0,1000}` | Status changes, and what they cost connections that share no room. |
| `ReactionStorm` | 300 members reacting to one message (`storeB/op`). |
| `SignInToken/members={100,1000}` | Signing in with a token into a large room. |
| `EventFollowers/followers={0,1,4}`, `EventDecodeMessage` | Extensions that read every message, such as `examples/search`'s index. |
| `LogRetained` | `retainedB/op`, with a store that keeps nothing. |
| `store`: `SQLiteApply/entries={4,64}` | One SQLite transaction. |
| `aprond/internal/durable`: `WriterBacklog` | A change's cost when the store writer has fallen behind. |
| `aprond/internal/roomlog`: `MessageRecord` | Encoding one message snapshot. |

### Over the network: `cmd/apron-hammer`

Run `aprond --debug-addr 127.0.0.1:6060 --messages-per-minute 0`, then
`go run ./cmd/apron-hammer -debug http://127.0.0.1:6060 -profile-dir <dir>
-scenarios <list>`. Read throughput, latency, "server allocs", and "heap
retained", and compare allocations with `go tool pprof -base
<dir>/<scenario>.allocs-before.pprof <dir>/<scenario>.allocs.pprof`. The
`bigroom` (posts/s and delivery to 200 or more readers) and `reactions`
(latency on one hot message) scenarios cover what the in-process benchmarks
found costly; [the README](cmd/apron-hammer/README.md) lists them all.

## What the API asks of a backend, and what it costs

The profiling that added these benchmarks checked whether the engine's API
forces a backend into costly patterns. It does not, as long as a backend
uses the forms meant for its shape:

- **Encode once, send to many.** Encode a frame once and hand the same
  `apron.Frame` to every recipient: `Server.Broadcast` (with a filter),
  `server.SendAll` (a backend's own member list), or `Conn.Send`. None
  copies the frame or allocates for each recipient. Encoding for each
  connection (`Notify`) costs an encoding each.
- **Send under your lock.** Sending queues and never blocks: a frame costs
  a queue slot, and the writers wake once the whole fan-out is queued, so
  holding a lock across a fan-out to thousands of connections takes
  microseconds. A stalled connection is dropped once its queue is full,
  without slowing the others.
- **Big results outside your lock.** `Reply.Reserve` fixes a reply's place
  in the connection's queue under the lock; the result is encoded and sent
  after it, in the same order as `Send` would have kept.
- **Stored records, sent as stored.** `HistoryResult.Raw` and
  `RoomView.Raw` send records a backend keeps encoded without decoding and
  encoding them again.
- **Users who share rooms.** `PublishUserTo` tells only the connections a
  backend names of a user's change; `PublishUser`'s filter visits every
  connection.
- **Read without copies.** `Conn.Identity`, `Request.Fields`, and
  `apron.Frame.Head` read what the connection or the frame holds; what they
  return is shared and only valid as documented.
