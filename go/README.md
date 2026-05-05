# `go/` — collector receiver

Go module: `github.com/agagniere/wasm4otel/go`
(package `wasm4otel`).

This package implements an OpenTelemetry Collector receiver that
instantiates a WebAssembly plugin via [wazero](https://github.com/tetratelabs/wazero)
and forwards the telemetry it produces to the next consumer in the
pipeline.

See the repository [README](../README.md) for the high-level picture.
This document covers the implementation details that matter when
embedding the receiver, debugging plugin loading, or extending the
host side of the ABI.

## Public API

```go
import wasm4otel "github.com/agagniere/wasm4otel/go"

factory := wasm4otel.NewFactory()
```

- `NewFactory() receiver.Factory` — returns the receiver factory to
  register in a collector distribution. Type name: `wasm4otel`.
  Currently only the **logs** signal is registered, at stability level
  `development`.
- `Config` — the YAML-mapped config struct.
- `DefaultConfig() component.Config` — supplies an empty `Config`.

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

`createLogs` in `factory.go` runs three steps when the collector
builds the pipeline:

1. `NewWasmOtelComponent` — validates config, opens a cancellable
   context, creates a wazero runtime, instantiates
   `wasi_snapshot_preview1`.
2. `ExposeFunctionsToGuest` — registers an `env` host module
   exporting `host_log` and `push_logs`.
3. `LoadPlugin` — reads the `.wasm` file from disk, instantiates the
   module (which runs `_start` or `_initialize`), and looks up
   exported functions: `start`, `stop`, `capabilities`,
   `consume_logs`, `consume_metrics`, `consume_traces`.

After construction, the next consumer is stored on the component and
the collector calls `Start` (which invokes the plugin's `start`) and
later `Shutdown` (which invokes `stop` and cancels the context).

## ABI strings used in this code

Three module/function-name strings appear in `wasm_otel_component.go`.
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

### `push_logs(ptr: i32, size: i32) -> i32`

Reads an OTLP-encoded `LogsData` protobuf from the guest's memory and
calls `nextConsumerLogs.ConsumeLogs`. Return codes:

| Code | Meaning                                                   |
| ---- | --------------------------------------------------------- |
| 0    | Success.                                                  |
| 1    | Could not read `(ptr, size)` from the guest's memory.     |
| 2    | `plog.ProtoUnmarshaler` failed to decode the payload.     |
| 3    | No logs consumer is wired (plugin pushing to a dead-end). |

`push_metrics` and `push_traces` are commented out in
`ExposeFunctionsToGuest`. Adding them is mostly mechanical (use
`pmetric` / `ptrace` unmarshallers and the corresponding consumer
interfaces) once metrics/traces signals are registered in the factory.

## Guest exports (what the host looks up)

| Export             | Called?                                                      |
| ------------------ | ------------------------------------------------------------ |
| `start`            | Yes, on `component.Start`.                                   |
| `stop`             | Yes, on `component.Shutdown`.                                |
| `capabilities`     | Looked up but not yet invoked.                               |
| `consume_logs`     | Looked up but not yet invoked.                               |
| `consume_metrics`  | Looked up but not yet invoked.                               |
| `consume_traces`   | Looked up but not yet invoked.                               |

The `consume_*` symbols are placeholders for plugins that act as
processors or exporters. Wiring them requires registering the
matching collector signals in `NewFactory`.

## Runtime configuration

The wazero runtime is built with `NewRuntimeConfigInterpreter` — pure
interpreter, no JIT. This trades throughput for a minimal,
dependency-free build. Switching to `NewRuntimeConfigCompiler` would
likely improve plugin throughput but pulls in platform-specific
codegen.

The module config in `LoadPlugin` does not redirect `stdout` / `stderr`
or override start functions. The commented-out lines in
`wasm_otel_component.go` show the wazero knobs available if you need
them.

## Dependencies

Pinned in `go.mod`:

- `github.com/tetratelabs/wazero` — wasm runtime.
- `go.opentelemetry.io/collector/{component,consumer,pdata,receiver}` —
  collector framework. `pdata/plog.ProtoUnmarshaler` is what decodes
  the bytes pushed by the guest.
- `go.uber.org/zap` — collector logger.

## Building and testing

```sh
go build ./...
go test ./...
```

There are no tests in the package today; adding integration tests
would mean instantiating a tiny `.wasm` fixture against a `consumertest`
sink.
