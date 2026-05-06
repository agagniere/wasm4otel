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
  `otel_pipeline_data`, and pushes through `push_logs`. The
  long-term plan is to add a poll/sleep loop so it acts like a real
  receiver.
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
