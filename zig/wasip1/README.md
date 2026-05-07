# `wasip1/` — `wasm32-wasi` reactor plugins

Plugins built for `wasm32-wasi` (preview 1) using the **reactor**
execution model. The host instantiates them once, calls `_initialize`,
then drives the lifecycle through `start` / `stop` / future
`consume_logs` etc. exports.

See [`../README.md`](../README.md) for the build system, shared
modules, and the host-ABI conventions that apply to every Zig
plugin. This document covers what WASIp1 buys over freestanding and
the gotchas specific to running under wazero.

## When to use this target

Pick `wasip1` whenever the plugin needs **anything** beyond pure
computation and the host-side `env` imports. The reachable WASI
surface includes:

- **Real clocks** — `std.Io.Clock.now(.real, io)` and
  `Clock.now(.monotonic, io)` resolve via `clock_time_get`.
- **Sleep / timers / poll** — anything routing through
  `poll_oneoff`. Required for batch flush intervals, rate limiting,
  retry backoff.
- **Stdout / stderr** — `fd_write` to fds 1 and 2. Useful as a
  fallback logger when running under `wasmtime` for tests.
- **Filesystem I/O** — `std.fs.*` against any directory the host
  pre-opens. The Go side does *not* preopen anything today, so this
  is theoretical until `LoadPlugin` calls `WithFSConfig(...)`.
- **Randomness** — `random_get`. Needed by anything generating
  trace IDs, span IDs, UUIDs.
- **Args / env vars** — `args_get` / `environ_get`. Wazero's
  `WithArgs` / `WithEnv` are not wired today.

If a plugin only needs `host_log` and `push_logs`, prefer
[`../freestanding/`](../freestanding/) — smaller binary, no
determinism caveats.

## Determinism gotchas under wazero

Wazero is reproducible-by-default. Until the embedder opts in, the
WASI calls a plugin can reach return frozen values rather than
hitting the OS:

| WASI call         | Default behaviour       | Wazero opt-in            |
| ----------------- | ----------------------- | ------------------------ |
| `clock_time_get`  | fixed `2022-01-01T00:00:00Z` (realtime) / 1 ns counter (monotonic) | `WithSysWalltime()`, `WithSysNanotime()` |
| `poll_oneoff`     | returns immediately     | `WithSysNanosleep()`     |
| `random_get`      | seeded deterministic stream | `WithRandSource(io.Reader)` |

A plugin that calls `Clock.now`, sleeps, or generates random bytes
silently gets stubbed values unless the host configured the matching
opt-in. If your plugin needs real (non-deterministic) values, ensure
the corresponding `With…` is wired on the wazero `ModuleConfig`
host-side. If a `time_unix_nano` is stuck in 2022 or trace IDs
collide across runs, this is the cause. See
[`../../go/WAZERO.md`](../../go/WAZERO.md) for the full table.

## Memory ownership of OTLP structs — use an arena

The `otel_pipeline_data` types come from `zig-protobuf`, whose
generated `deinit(self, allocator)` calls `allocator.free` on **every
non-empty `[]const u8`** in the struct (and recurses into
submessages). That is correct after a `decode` — the decoder does
allocate every string from the same allocator. It is **wrong** for a
struct you assemble by hand with string literals, `build_info`
slices, `@src().fn_name`, etc.: those pointers live in the wasm data
section and were never allocated. `wasm_allocator.free(literal)` does
not crash but silently corrupts the brk-allocator's free-list, after
which the protobuf encoder's per-submessage `Io.Writer.Allocating`
temp buffers come back overlapping live data — the *next* encode
emits a malformed wire message and the host's `UnmarshalLogs` rejects
it with something like `proto: Link: illegal field=0 (tag=1, pos=N)`.

The cure is to drive lifetime with an `ArenaAllocator` and never call
the generated `deinit` on hand-built structs:

```zig
var arena: std.heap.ArenaAllocator = .init(std.heap.wasm_allocator);
defer arena.deinit();
const alloc = arena.allocator();

while (running) {
    defer _ = arena.reset(.retain_capacity);
    const logs = try generateLogs(alloc, io);
    try pushLogs(alloc, logs);
    // No `logs.deinit(alloc)` — the literals inside would corrupt
    // the underlying allocator. `arena.reset` releases everything.
    try host.interruptibleSleep(.fromSeconds(2));
}
```

Decoded structs (from `LogsData.decode(...)`) *are* `deinit`-safe,
but the arena pattern subsumes them too — pass the arena to `decode`,
process the result, `arena.reset()`, no `deinit` needed.

The same trap applies to freestanding plugins that hand-build OTLP
structs (e.g. [`../freestanding/one_log.zig`](../freestanding/one_log.zig)) —
it is just less likely to bite there, because a one-shot push tears
the module down before the corruption matters.

## Reactor entrypoint

`exe.wasi_exec_model = .reactor` makes wasm-ld require an
`_initialize` export. Zig 0.16 does not synthesize one, so every
plugin in this folder declares its own.

The host's `WithStartFunctions("_start", "_initialize")` covers both
modules, so freestanding (`_start`) and wasip1 (`_initialize`)
plugins instantiate the same way from the host's point of view.

## Plugins in this folder

- **`log_generator.zig`** — emits a batch of OTLP `LogRecord`s with
  real `time_unix_nano` / `observed_time_unix_nano`, encodes via
  `otel_pipeline_data`, and pushes through `push_logs`. Loops a
  fixed number of times, pacing batches with `host.interruptibleSleep`.
  The host runs `start.Call(...)` on a dedicated goroutine, so the
  loop doesn't block collector startup. `Shutdown` cancels the
  component's context, which makes `interruptible_sleep_ms` return
  non-zero on its `select`'s `<-ctx.Done()` arm; the Zig wrapper
  surfaces that as `error.Interrupted`, the loop's `try` unwinds
  the iteration's `defer`s, `start` returns, the goroutine drains,
  and `stop` runs. Shutdown latency is sub-millisecond regardless
  of the sleep interval, and the path doesn't depend on wazero's
  ctx-cancellation semantics or on `WithSysNanosleep` being wired.
- **`severity_parser.zig`** — small textual-severity → OTLP
  `SeverityNumber` parser with `test {}` blocks. Demonstrates the
  `zig build test -fwasmtime` flow; see the *Tests* section of
  [`../README.md`](../README.md). Note the
  `if (builtin.is_test)` branch on `std_options` that swaps
  `host_log.logFn` for the default stderr logger so the test binary
  doesn't ask wasmtime for the absent `env.host_log` import.

## Adding a plugin

Append an entry to `wasip1_sources` in [`../build.zig`](../build.zig):

```zig
const wasip1_sources: []const OtelPlugin = &.{
    .{ .filename = "log_generator.zig",   .symbols = &.{ "start", "stop" } },
    .{ .filename = "severity_parser.zig", .symbols = &.{"start"}, .tests = true },
    // .{ .filename = "my_plugin.zig",    .symbols = &.{ "start", "stop" } },
};
```

`tests = true` opts the source into `zig build test -fwasmtime`.
Both `hostlog` and `otel_pipeline_data` are wired by default.
