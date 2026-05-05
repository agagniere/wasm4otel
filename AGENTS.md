# AGENTS

This file provides guidance to LLMs when working with code in this repository.

## What this project is

`wasm4otel` is an OpenTelemetry Collector receiver that runs WebAssembly plugins as telemetry sources. The host (Go) embeds [wazero](https://github.com/tetratelabs/wazero) and instantiates a guest `.wasm` plugin; the guest produces OTel logs/metrics/traces and pushes them back through the collector pipeline.

The repo has three top-level pieces:

- `go/` — the host: an `otel_receiver.Factory` that loads a WASM module from `config.path`, wires host imports, and forwards plugin output to the next consumer.
- `zig/` — example guest plugins built in Zig (one freestanding, one WASI preview1 reactor) plus shared modules.

## Host/guest ABI

The contract between Go and the guest is defined imperatively in `go/wasm_otel_component.go`. Keep both sides in sync when changing it.

Host imports exposed in the `env` module (called from the guest):
- `host_log(level: i32, ptr, size)` — level matches `go.uber.org/zap/zapcore.Level` (debug=-1, info=0, …). The Zig side mirrors this in `zig/src/log.zig`'s `LogLevel` enum.
- `push_logs(ptr, size) -> i32` — bytes are an OTLP-encoded `LogsData` protobuf. Returns 0 on success, non-zero error code otherwise (1 = bad memory read, 2 = decode failure, 3 = no downstream consumer).
- `push_metrics` / `push_traces` are stubbed in the Go side and not yet wired.

Guest exports the host calls:
- `start()` — invoked from `Start(ctx, host)`. The log generator does its work synchronously here.
- `stop()` — invoked from `Shutdown(ctx)`.
- `_initialize` (WASI reactor) or `_start` (freestanding) — wazero invokes one of these on instantiation depending on the module type.
- `capabilities`, `consume_logs`, `consume_metrics`, `consume_traces` — looked up by the host but not yet called; reserved for processor/exporter-style plugins.

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

The package exports `NewFactory()` for use inside an OTel Collector distribution; there is no standalone binary in this repo.

## Things that are easy to get wrong

- The proto-generated Zig sources under `zig/src/opentelemetry/proto/` are gitignored and produced by `gen-proto`. Don't hand-edit them; they're regenerated from the pinned `otelproto` dep.
- `wasi_exec_model = .reactor` on wasip1 plugins is what causes wazero to call `_initialize` instead of `_start`. The Zig source still has to `@export` the function under that name (see `wasip1/log_generator.zig`).
- `host_log` log levels are zap levels, not `std.log.Level` values. `zig/src/log.zig` has `LogLevel.fromStd` to bridge them — use `hostLog`/`hostLogFormat`/`logFn` from that module rather than calling `host_log` directly.
- `push_logs` expects an OTLP `LogsData` protobuf payload; the encoding lives in the generated `Logs` types re-exported through `zig/src/pipeline.zig` (`otel_pipeline_data` module).
- `wasm_otel_component.go` currently uses `wazero.NewRuntimeConfigInterpreter()` (interpreter, not compiler) — performance-sensitive changes should account for that.
