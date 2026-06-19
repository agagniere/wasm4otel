# AGENTS

This file provides guidance to LLMs when working with code in this repository.

## What this project is

`wasm4otel` is a set of OpenTelemetry Collector components that run WebAssembly plugins. The host (Go) embeds [wazero](https://github.com/tetratelabs/wazero) and instantiates a guest `.wasm` plugin; the guest can act as a receiver (drives its own loop, pushes telemetry), a processor (transforms each batch handed to it), or an exporter (terminally consumes each batch).

The repo has two top-level pieces:

- `go/` — the host. Single Go module (`github.com/agagniere/wasm4otel/go`) with four packages: a shared `wasm4otel` package holding the `Component` type and host imports, plus three role packages (`go/receiver`, `go/processor`, `go/exporter`) each exposing `NewFactory()`.
- `zig/` — example guest plugins built in Zig (one freestanding, one WASI preview1 reactor) plus shared modules.

## Host/guest ABI

The contract between Go and the guest is defined imperatively in `go/component.go`. Keep both sides in sync when changing it.

Host imports exposed in the `env` module (called from the guest):
- `host_log(level: i32, ptr, size)` — level matches `go.uber.org/zap/zapcore.Level` (debug=-1, info=0, …). The Zig side mirrors this in `zig/src/log.zig`'s `LogLevel` enum.
- `push_logs(ptr, size) -> i32` / `push_metrics(ptr, size) -> i32` / `push_traces(ptr, size) -> i32` — bytes are an OTLP-encoded `LogsData` / `MetricsData` / `TracesData` protobuf. Returns 0 on success, non-zero error code otherwise (1 = bad memory read, 2 = decode failure, 3 = no downstream consumer, 4 = downstream consumer rejected the batch). A processor plugin calls these from inside its `consume_<signal>` to forward the transformed batch; an exporter doesn't call them (or calls them knowing the host returns 3).
- `interruptible_sleep_ms(ms: u32) -> u32` — sleeps for up to `ms` milliseconds. Returns 0 when the duration elapsed, non-zero when the component's context fires (Shutdown). The Zig wrapper in `zig/src/host.zig` (`host` module) surfaces this as `interruptibleSleep(std.Io.Duration) error{Interrupted}!void`. Works in both freestanding and wasip1 — does not depend on any WASI plumbing.
- `get_config(ptr: u32, size: u32) -> u32` — hands the YAML `plugin_config` map to the guest as a JSON document. Returns the document's true byte length. If `size >= true_size`, writes the bytes at `ptr`; if `size < true_size`, writes nothing so the guest can re-allocate and call again. Returns 0 when YAML didn't set `plugin_config`. Probe with `(0, 0)` to learn the size without touching memory. The Zig wrapper `host.getConfig(allocator)` returns `?[]u8` (caller frees).

Guest exports the host calls:
- `start() -> i32` — invoked from `Start(ctx, host)`. Returns `0 = success`, `1 = generic failure`, `2 = invalid user-provided config`; the host surfaces non-zero as an `error` from `Start` (processor/exporter) or cancels the component context (receiver). Guests should `host_log` their own detail before returning non-zero. Zig plugins declare it as `export fn start() guest.StartResult` against the non-exhaustive enum in `zig/src/guest.zig`. Empty results (`() -> ()`) are accepted as success for forward-compat but the convention is the i32 return. In receiver mode start runs on a dedicated goroutine; in processor/exporter mode it runs synchronously and must return promptly.
- `stop()` — invoked from `Shutdown(ctx)`. In receiver mode, the host waits for the `start` goroutine to drain first.
- `_initialize` (WASI reactor) or `_start` (command / freestanding) — the plugin code declares one or the other; wasm-ld requires whichever the `wasi_exec_model` selected. `LoadPlugin` passes both names to `ModuleConfig.WithStartFunctions(...)` and wazero calls whichever is present.
- `consume_logs(ptr, size) -> i32` / `consume_metrics(ptr, size) -> i32` / `consume_traces(ptr, size) -> i32` — required for processor/exporter mode on the matching signal. The host invokes one of these once per incoming batch with the host-allocated buffer pointer and size; returns 0 on success, non-zero for plugin-side errors.
- `wasm4otel_alloc(size: u32) -> u32`, `wasm4otel_free(ptr: u32, size: u32)` — required alongside `consume_*` for processor/exporter mode. The host calls `wasm4otel_alloc` to reserve a region in the guest's linear memory, writes the OTLP payload into it, invokes the `consume_*` export, then calls `wasm4otel_free`. Returning 0 from `wasm4otel_alloc` signals failure; the host treats 0 as a non-trapping skip and does not call `wasm4otel_free`. Names are prefixed because Rust + wasm32-wasi links wasi-libc, which already defines `free`; an unprefixed export collides at link time.

The role each plugin plays is decided by which factory the operator registers in the OTel collector YAML — `receivers:`, `processors:`, or `exporters:` — not by the plugin itself. Each factory advertises all three signals (logs, metrics, traces) at startup; the per-signal `Validate<Signal>Export` checks at create-time that the plugin exports the matching `consume_<signal>` for the pipeline section it sits under. A plugin that only speaks one signal fails fast with a clear error if wired into the wrong section.

## Building and running

### Zig guests (canonical build)

From `zig/`:

```sh
zig build                # builds all freestanding + wasip1 plugins into zig/zig-out/bin/
zig build gen-proto      # regenerate src/opentelemetry/proto/**/*.pb.zig from otelproto dep
```

`build.zig` declares two target sets driven by the `freestanding_sources` / `wasip1_sources` arrays at the bottom — add a new plugin by appending to the right list with its filename and the symbols to export. The build emits `_initialize` (reactor) for wasip1 modules and a regular `_start` for freestanding ones.

Requires Zig **0.16.0+**. Dependencies (`protobuf`, `otelproto`) are pinned in `build.zig.zon`; `zig/UPDATE.md` has the `zig fetch --save` commands to bump them.

The standalone `zig/Makefile` is a legacy single-file workflow (build one `.zig` to a wasm component via `wasm-tools component new`). It is **not** what `build.zig` does and is unused by the current plugins.

### Go host

From `go/`:

```sh
go build ./...
go test ./...
```

Each role subpackage exports `NewFactory()` for use inside an OTel Collector distribution — `github.com/agagniere/wasm4otel/go/receiver`, `.../go/processor`, `.../go/exporter`. The shared `github.com/agagniere/wasm4otel/go` package is not imported directly by operators; it holds the `Component` type and the host imports the role packages share. There is no standalone binary in this repo.

## Things that are easy to get wrong

- The proto-generated Zig sources under `zig/src/opentelemetry/proto/` are gitignored and produced by `gen-proto`. Don't hand-edit them; they're regenerated from the pinned `otelproto` dep.
- `wasi_exec_model = .reactor` on wasip1 plugins is what makes wasm-ld require `_initialize` as the entry symbol (vs `_start` in command/freestanding). Zig 0.16 does not auto-emit either — the plugin source must `@export` the function (see `wasip1/log_generator.zig`). Wazero itself doesn't auto-detect; `LoadPlugin` passes both names to `WithStartFunctions(...)` so whichever the plugin declares gets called.
- `host_log` log levels are zap levels, not `std.log.Level` values. `zig/src/log.zig` has `LogLevel.fromStd` to bridge them — use `hostLog`/`hostLogFormat`/`logFn` from that module rather than calling `host_log` directly.
- `push_logs` expects an OTLP `LogsData` protobuf payload; the encoding lives in the generated `Logs` types re-exported through `zig/src/pipeline.zig` (`otel_pipeline_data` module).
- `component.go` currently uses `wazero.NewRuntimeConfigInterpreter()` (interpreter, not compiler) — performance-sensitive changes should account for that.
- Processor/exporter plugins are single-occupant — `Component.callMu` serializes every `Call()` into the wasm instance, because wazero modules are not safe for concurrent calls. The receiver-mode `start()` goroutine runs without the mutex (it owns the instance for its lifetime), so a plugin cannot meaningfully play both receiver and processor roles in the same instance.
- After any guest trap (`wasm4otel_alloc`, `consume_*`, `wasm4otel_free`, `stop`), `Component.broken` latches and further guest entries fail closed. The collector will see a stream of errors; we don't try to recover the instance.
