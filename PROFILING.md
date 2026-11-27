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
