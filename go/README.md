# `go/` — collector host

Go module: `github.com/agagniere/wasm4otel/go`.

This module implements three OpenTelemetry Collector components —
receiver, processor, and exporter — that instantiate a WebAssembly
plugin via [wazero](https://github.com/tetratelabs/wazero) and route
telemetry through the pipeline. The shared `Component` type lives in
the root package and the three role factories live in subpackages.

See the repository [README](../README.md) for the high-level picture.
This document covers the implementation details that matter when
embedding any of the factories, debugging plugin loading, or extending
the host side of the ABI.

## Layout

```
go/
├── component.go                       package wasm4otel
├── config.go                          package wasm4otel
├── receiver/factory.go                package wasm4otelreceiver
├── processor/factory.go               package wasm4otelprocessor
└── exporter/factory.go                package wasm4otelexporter
```

The shared `wasm4otel` package holds the wazero plumbing, the host
imports (`host_log`, `push_logs`, `push_metrics`, `push_traces`,
`interruptible_sleep_ms`), the guest-export lookups, the `Component`
type and its `Start`/`Shutdown`/`ConsumeLogs`/`ConsumeMetrics`/
`ConsumeTraces`/`Capabilities` methods. The role packages are thin —
each exports `NewFactory()` and `createLogs` / `createMetrics` /
`createTraces` hooks that pick a `ComponentMode` and validate the
exports the matching signal needs.

## Public API

```go
import (
    wasm4otelreceiver  "github.com/agagniere/wasm4otel/go/receiver"
    wasm4otelprocessor "github.com/agagniere/wasm4otel/go/processor"
    wasm4otelexporter  "github.com/agagniere/wasm4otel/go/exporter"
)

wasm4otelreceiver.NewFactory()  // receiver.Factory
wasm4otelprocessor.NewFactory() // processor.Factory
wasm4otelexporter.NewFactory()  // exporter.Factory
```

All three use the type name `wasm4otel`; the YAML section
(`receivers:`/`processors:`/`exporters:`) decides the role.
Currently only the **logs** signal is registered for each role, at
stability level `development`.

From the shared `wasm4otel` package:

- `Config` — the YAML-mapped config struct.
- `DefaultConfig() component.Config` — supplies an empty `Config`.
- `Load(cfg, logger, mode) (*Component, error)` — the one-shot
  factory entrypoint: builds the component, registers host imports,
  loads the plugin, and verifies mode-mandatory exports
  (`wasm4otel_alloc` / `wasm4otel_free` for processor/exporter).
  Each role's per-signal `createX` calls this.
- `Component` — the host-side type each factory builds and returns;
  satisfies `receiver.{Logs,Metrics,Traces}`,
  `processor.{Logs,Metrics,Traces}`, and
  `exporter.{Logs,Metrics,Traces}` depending on the `ComponentMode`
  passed to `Load` / `NewComponent`.
- `ComponentMode` — `ModeReceiver` / `ModeProcessor` / `ModeExporter`.
  Implements `fmt.Stringer` for use in error messages.

## Configuration

```yaml
receivers:
  wasm4otel:
    path: /path/to/plugin.wasm   # required
    plugin_config:               # optional, free-form map
      key: value
```

| Field           | Type                     | Notes                                                                |
| --------------- | ------------------------ | -------------------------------------------------------------------- |
| `path`          | string (required)        | Filesystem path to the `.wasm` module. `Validate` errors if empty.   |
| `plugin_config` | `map[string]interface{}` | Reserved for passing arbitrary config to the guest. Not yet wired.   |

## Lifecycle

Every role's per-signal `createX` calls
`wasm4otel.Load(cfg, logger, mode)`, which runs the three
signal-independent setup steps:

1. `NewComponent` — validates config, derives a cancellable context
   **from `context.Background()`** (deliberately *not* the framework's
   create-phase ctx, which can be cancelled the moment `createX`
   returns), creates a wazero runtime, instantiates
   `wasi_snapshot_preview1`.
2. `ExposeFunctionsToGuest` — registers an `env` host module
   exporting `host_log`, `push_logs`, `push_metrics`, `push_traces`,
   and `interruptible_sleep_ms`.
3. `LoadPlugin` — reads the `.wasm` file from disk, instantiates the
   module (which runs `_start` or `_initialize`), stores the instance
   for later memory access, and looks up exported functions: `start`,
   `stop`, `consume_logs`, `consume_metrics`, `consume_traces`,
   `wasm4otel_alloc`, `wasm4otel_free`.

`Load` also verifies the signal-independent export contract for the
mode: for processor/exporter modes it checks `HasAllocFree()` and
returns a "wasm4otel <role>: plugin must export wasm4otel_alloc and
wasm4otel_free" error if missing (a guest without them is genuinely
incomplete). Receiver mode skips that check — receivers don't consume.

The role package then:

- Validates the signal-specific export. The processor and exporter
  `createX` call `Component.Validate<Signal>Export()`, which checks
  `HasConsume<Signal>()` and returns a "wasm4otel <role>: plugin does
  not export consume_<signal>; it does not support the <signal>
  signal" error if missing. A guest missing this is fine in general —
  it just doesn't speak that signal, and should be wired into a
  different pipeline. Receiver mode skips signal validation; it only
  needs `start`.
- Stores the downstream consumer in
  `component.NextConsumer{Logs,Metrics,Traces}` (receiver and
  processor only — the exporter is terminal).

After that, the framework calls `Start`/`Shutdown` and (for processor
and exporter) `Consume<Signal>`/`Capabilities`. Their behavior
branches on `mode`:

| Method          | `ModeReceiver`                                                                                                  | `ModeProcessor` / `ModeExporter`                                                                  |
|-----------------|-----------------------------------------------------------------------------------------------------------------|---------------------------------------------------------------------------------------------------|
| `Start`         | Spawns a goroutine that calls `start()`; returns immediately. The goroutine owns the instance until shutdown.   | Calls `start()` synchronously under `callMu`. Must return promptly.                               |
| `Shutdown`      | Cancels the context (so any blocking host import unwinds), waits for the `start` goroutine, then calls `stop()`. | Cancels the context, calls `stop()`. No goroutine to wait for.                                    |
| `Consume<Sig>`  | Not called.                                                                                                     | Marshals the batch and runs the `wasm4otel_alloc → write → consume_<sig> → wasm4otel_free` dance. |

## ABI strings used in this code

Three module/function-name strings appear in `component.go`.
What each one means:

### `"env"` — toolchain convention

`NewHostModuleBuilder("env")` registers a host module under the
literal string `"env"`. Wazero treats it like any other name; the
guest decides the namespace and the host has to match. The reason
`"env"` is the de facto choice is that LLVM-based toolchains (Clang,
Rust, Zig) default to it as the import module for any `extern`
function declared without an explicit one. Pick a different name on
both sides and it works the same.

### `_start` / `_initialize` — WASIp1 lifecycle

The WASIp1 spec assigns two entrypoint names depending on the module
type:

| Module type            | Entrypoint     |
| ---------------------- | -------------- |
| Command (one-shot)     | `_start`       |
| Reactor (long-lived)   | `_initialize`  |

Wazero does **not** auto-detect which one a module exports. At
instantiation it calls whatever is listed in
`ModuleConfig.WithStartFunctions(...)`, which defaults to `["_start"]`.
A reactor module instantiated with the default config has its
`_initialize` skipped silently.

`LoadPlugin` therefore passes both names explicitly:

```go
config := wazero.NewModuleConfig().
    WithStartFunctions("_start", "_initialize")
```

Wazero calls each one that's present and skips ones that aren't, so a
single config handles both module flavours.

After instantiation, neither is called again — `start()` and `stop()`
(the function names this project chose, no spec involved) are looked
up explicitly via `instance.ExportedFunction(...)` and called on
collector lifecycle events.

### `"wasi_snapshot_preview1"` — the WASI module name

In `wasi_snapshot_preview1.MustInstantiate`, the string is the
namespace WASIp1 imports use, fixed by the WASI spec. Guests declare
their WASI imports under that exact module name; the
`imports/wasi_snapshot_preview1` package registers a host module to
resolve them.

## Host function signatures via Go reflection

The host functions passed to `WithFunc(...)` look like ordinary Go
functions — `logToZap` and `outboundLogs` don't manually parse wasm
values or stack frames. Wazero figures out the wasm signature from
the Go signature using reflection:

- The first parameter may be `context.Context`. If present, wazero
  passes the call's context.
- The next parameter may be `api.Module`. If present, wazero passes
  the calling module — used for `module.Memory().Read(ptr, size)`.
- Remaining parameters and the return value are matched 1:1 against
  wasm value types: `int32`/`uint32` ↔ `i32`, `int64`/`uint64` ↔ `i64`,
  `float32` ↔ `f32`, `float64` ↔ `f64`. No other Go types are valid.

Memory and string conversions are not automatic — pointer/length
pairs come through as `i32`s and the host calls
`module.Memory().Read(...)` to materialize them as `[]byte`.

## Host imports (what the guest can call)

Exported under the `env` module.

### `host_log(level: i32, ptr: i32, size: i32)`

Reads `size` bytes at `ptr` from the plugin's linear memory and logs
them through the collector's zap logger, tagged with
`plugin=<config.path>`. `level` is a
[`zapcore.Level`](https://pkg.go.dev/go.uber.org/zap/zapcore#Level)
value — `-1` for debug, `0` info, `1` warn, `2` error, etc.

### `push_logs(ptr: i32, size: i32) -> i32`<br>`push_metrics(ptr: i32, size: i32) -> i32`<br>`push_traces(ptr: i32, size: i32) -> i32`

Each reads an OTLP-encoded `LogsData` / `MetricsData` / `TracesData`
protobuf from the guest's memory and calls the matching downstream
`Consume<Signal>`. Return codes (uniform across the three):

| Code | Meaning                                                       |
| ---- | ------------------------------------------------------------- |
| 0    | Success.                                                      |
| 1    | Could not read `(ptr, size)` from the guest's memory.         |
| 2    | The matching `ProtoUnmarshaler` failed to decode the payload. |
| 3    | No downstream consumer is wired (plugin pushing to dead-end). |
| 4    | Downstream `Consume<Signal>` returned an error.               |

### `interruptible_sleep_ms(ms: i32) -> i32`

`select`s on `time.After(ms * Millisecond)` versus the component's
context. Returns `0` when the duration elapses normally, `1` when the
context fires (i.e. `Shutdown` ran). This is the cooperative-shutdown
hook the plugin's loop hangs off — see `zig/src/host.zig` for the
guest-side wrapper that turns the non-zero return into
`error.Interrupted`. The underlying `select` resolves immediately when
the context fires, so shutdown latency is sub-millisecond regardless
of the requested sleep duration.

## Guest exports (what the host looks up)

| Export             | Called?                                                                                                                                |
| ------------------ | -------------------------------------------------------------------------------------------------------------------------------------- |
| `start`            | Yes, on `Component.Start`. Required for receivers; optional for processor/exporter.                                                    |
| `stop`             | Yes, on `Component.Shutdown`. Optional in every mode.                                                                                  |
| `consume_logs`     | Yes for processor/exporter on the logs pipeline. Invoked once per incoming batch from `Component.ConsumeLogs`.                         |
| `consume_metrics`  | Yes for processor/exporter on the metrics pipeline. Invoked once per incoming batch from `Component.ConsumeMetrics`.                   |
| `consume_traces`   | Yes for processor/exporter on the traces pipeline. Invoked once per incoming batch from `Component.ConsumeTraces`.                     |
| `wasm4otel_alloc`  | Yes for processor/exporter mode. Called before each `consume_*` to reserve a buffer in the guest's linear memory.                      |
| `wasm4otel_free`   | Yes for processor/exporter mode. Called after each `consume_*` to release the buffer.                                                  |

### `wasm4otel_alloc(size: u32) -> u32`, `wasm4otel_free(ptr: u32, size: u32)`

The host needs a way to put OTLP bytes into the guest's linear memory
without trampling whatever the guest's allocator is doing. Each
processor/exporter plugin exports two small functions backed by its
own allocator (e.g. Zig's `std.heap.wasm_allocator`):

- `wasm4otel_alloc(size)` — reserve `size` bytes, return the offset,
  or `0` on failure. `0` is reserved as a sentinel because no real
  wasm allocator returns it (linear memory's low region holds `.data`
  / `.rodata`). The host treats `0` as a non-trapping skip and does
  **not** call `wasm4otel_free` on it.
- `wasm4otel_free(ptr, size)` — release the region. The host passes
  `size` back so the guest's allocator doesn't need a per-block
  header.

The host's per-batch sequence is `wasm4otel_alloc → memory.Write →
consume_<signal> → wasm4otel_free`, all under `Component.callMu`. If
`consume_<signal>` traps, the host skips `wasm4otel_free` (the
instance is poisoned and any further call may trap again or behave
undefined-ly) and latches `Component.broken`.

### `consume_logs(ptr: i32, size: i32) -> i32`<br>`consume_metrics(ptr: i32, size: i32) -> i32`<br>`consume_traces(ptr: i32, size: i32) -> i32`

Invoked once per incoming batch on the matching signal pipeline.
`(ptr, size)` refers to a buffer the host just wrote into the guest's
linear memory via `wasm4otel_alloc`. The guest must not retain the
pointer past the call — the host calls `wasm4otel_free` as soon as
`consume_<signal>` returns. Return code `0` for success, non-zero for
plugin-side errors; the host surfaces non-zero rcs as a
`fmt.Errorf` and the framework treats it as a batch failure. A
processor plugin typically decodes the payload, transforms the batch,
re-encodes, and forwards via `push_<signal>` before returning `0`.

## Runtime configuration

The wazero runtime is built with `NewRuntimeConfigInterpreter` — pure
interpreter, no JIT. This trades throughput for a minimal,
dependency-free build. Switching to `NewRuntimeConfigCompiler` would
likely improve plugin throughput but pulls in platform-specific
codegen.

The module config in `LoadPlugin` does not redirect `stdout` / `stderr`
or override start functions. The commented-out lines in
`component.go` show the wazero knobs available if you need them.

## Dependencies

Pinned in `go.mod`:

- `github.com/tetratelabs/wazero` — wasm runtime.
- `go.opentelemetry.io/collector/{component,consumer,pdata,receiver,processor,exporter}` —
  collector framework. The three `pdata/p{log,metric,trace}.{ProtoMarshaler,ProtoUnmarshaler}`
  encode and decode the bytes that cross the host/guest boundary.
- `go.uber.org/zap` — collector logger.

## Building and testing

```sh
go build ./...
go test ./...
```

There are no tests in the package today; adding integration tests
would mean instantiating a tiny `.wasm` fixture against a `consumertest`
sink.
