# apron-hammer

A load generator for an Apron server. Each scenario drives many concurrent
WebSocket clients with ordinary protocol traffic, then reports throughput,
latency percentiles, errors, and server disconnects. Against `aprond` started
with `-debug-addr`, it also samples the server's heap and goroutines every
second and compares what the server retains before and after each scenario.

```sh
# Terminal 1: the server, with pprof and expvar on a separate listener.
go run ./cmd/aprond --debug-addr 127.0.0.1:6060

# Terminal 2: every scenario, 50 clients, 10 seconds each.
go run ./cmd/apron-hammer -profile-dir /tmp/apron-profiles
```

Scenarios run in order against the same server, so the state earlier ones
leave behind (messages, threads, uploads) is part of the load for later ones.
Restart the server between runs you want to compare. A scenario that needs a
capability the server's `server` frame does not list is skipped: `activity`
needs `activity`, `history` needs `history` and `rooms`, `threads` needs
`rooms`, `edits` needs `edit` and `reactions`, `embeds` needs
`embed:upload` and `embed:stream`, and `ext` needs `edit` and `ext`.

| Scenario   | Load |
|------------|------|
| `churn`    | connect, sign in as a guest, and disconnect as fast as possible; each guest's join to `general` at sign-in and its leave on disconnect are logged memberships |
| `flood`    | every client posts to `general` with one request in flight; checks each connection receives each room's message snapshots in ascending `log_id`, ignoring transient notices |
| `activity` | every client sends typing `activity` to `general` as a notification, without an `id`, every 250 ms; reports the latency of each client's own broadcast and the share of broadcasts others receive |
| `slow`     | half the clients stop reading while the rest post 4 KiB messages; the server should drop the stalled ones |
| `history`  | seed a room with 20,000 messages, then page it with `limit` 100 and 1000 |
| `threads`  | create 1,000 threads with `room_set`, then concurrently `room_list` them (`filter: "not_joined"`) and the joined rooms with `members: true`, or sign in with `room_list` (`filter: "joined"`, `members: true`) pipelined behind `auth`, then `room_join` and `room_leave` a thread; a listing without `users` is accepted |
| `edits`    | post, edit three times, react, clear, and delete, in a loop |
| `embeds`   | 64 KiB uploads read back from their file URLs, and two-second live streams read while they are written |
| `ext`      | saves messages through the `ext` merge's edge cases and checks each snapshot: empty values dropped, an integer beyond 2^53 kept exactly, two saves of different keys sent back to back both kept, `"ext": {}` and a save without `ext` changing nothing, `null` kept, and no `ext` on a tombstone |
| `signin`   | signs guests in over and over and checks that only the `server` frame and transient notices arrive before the `auth` result |

Flags:

- `-server 127.0.0.1:8080`, `-origin <origin>` (none by default)
- `-debug http://127.0.0.1:6060`: the `-debug-addr` listener; empty skips server monitoring
- `-scenarios churn,flood` or `all`; `-list` prints them
- `-clients 50`, `-duration 10s`, `-body-bytes 200` (flood message size)
- `-profile-dir <dir>`: saves `<scenario>.cpu.pprof` for the measured window,
  and `heap`, `allocs`, and `goroutine` profiles after it
  (`allocs-before` too, for `go tool pprof -base`)
- `-v`: print each server sample

Read a profile with, for example,
`go tool pprof -top -cum /tmp/apron-profiles/flood.cpu.pprof`.

The client and server share the machine's CPUs when run together, so a
client that falls behind its connection's outgoing queue is disconnected by
the server as a slow consumer. The report counts these as server disconnects.

Frames that break the protocol, such as a reply for an unknown `id`, a
notification carrying an `id`, or snapshots out of order, are counted as
protocol violations, with a few examples, in each scenario's report.
