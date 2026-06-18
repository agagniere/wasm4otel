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
- `push_logs(ptr, size) -> i32` — bytes are an OTLP-encoded `LogsData` protobuf. Returns 0 on success, non-zero error code otherwise (1 = bad memory read, 2 = decode failure, 3 = no downstream consumer, 4 = downstream consumer rejected the batch). A processor plugin calls this from inside its `consume_logs` to forward the transformed batch; an exporter doesn't call it (or calls it knowing the host returns 3).
- `interruptible_sleep_ms(ms: u32) -> u32` — sleeps for up to `ms` milliseconds. Returns 0 when the duration elapsed, non-zero when the component's context fires (Shutdown). The Zig wrapper in `zig/src/host.zig` (`host` module) surfaces this as `interruptibleSleep(std.Io.Duration) error{Interrupted}!void`. Works in both freestanding and wasip1 — does not depend on any WASI plumbing.
- `push_metrics` / `push_traces` are stubbed in the Go side and not yet wired.

Guest exports the host calls:
- `start()` — invoked from `Start(ctx, host)`. In receiver mode it runs on a dedicated goroutine so a long-running loop doesn't block collector startup. In processor/exporter mode it runs synchronously and must return promptly so the pipeline can begin delivering batches.
- `stop()` — invoked from `Shutdown(ctx)`. In receiver mode, the host waits for the `start` goroutine to drain first.
- `_initialize` (WASI reactor) or `_start` (command / freestanding) — the plugin code declares one or the other; wasm-ld requires whichever the `wasi_exec_model` selected. `LoadPlugin` passes both names to `ModuleConfig.WithStartFunctions(...)` and wazero calls whichever is present.
- `consume_logs(ptr, size) -> i32` — required for processor/exporter mode on the logs signal. The host invokes this once per incoming batch with the host-allocated buffer pointer and size; returns 0 on success, non-zero for plugin-side errors.
- `consume_metrics`, `consume_traces` — looked up by the host but the matching factories for metrics/traces are not yet registered.
- `wasm4otel_alloc(size: u32) -> u32`, `wasm4otel_free(ptr: u32, size: u32)` — required alongside `consume_*` for processor/exporter mode. The host calls `wasm4otel_alloc` to reserve a region in the guest's linear memory, writes the OTLP payload into it, invokes the `consume_*` export, then calls `wasm4otel_free`. Returning 0 from `wasm4otel_alloc` signals failure; the host treats 0 as a non-trapping skip and does not call `wasm4otel_free`. Names are prefixed because Rust + wasm32-wasi links wasi-libc, which already defines `free`; an unprefixed export collides at link time.

The role each plugin plays is decided by which factory the operator registers in the OTel collector YAML — `receivers:`, `processors:`, or `exporters:` — not by the plugin itself. The factory validates at create-time that the plugin exports the symbols its role needs (`start` for receiver; `consume_logs`/`wasm4otel_alloc`/`wasm4otel_free` for processor/exporter on the logs signal).

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
