# wasm4otel

An OpenTelemetry Collector receiver that runs **WebAssembly plugins**
as sources of telemetry. Write your data source once in any language
that compiles to wasm, drop the `.wasm` file into the collector's
config, and it streams logs (and eventually metrics and traces) into
the rest of your pipeline.

> ⚠️ **Status: proof of concept.** The host/guest ABI is hand-rolled
> and only logs are wired end-to-end. APIs will change.

## Why?

OpenTelemetry Collector components are written in Go and compiled into
the collector binary. That makes simple "scrape this thing and turn it
into OTLP" use cases heavier than they need to be: you fork a
distribution, vendor a module, and ship a new build for every change.

This project explores a lighter path:

- The collector loads a `.wasm` plugin at startup — no rebuild of the
  collector itself.
- Plugins run in a sandbox (wazero, pure-Go interpreter): no syscalls
  the host doesn't grant, deterministic memory.
- Plugins can be written in any language with a wasm backend. Zig is
  used here because it has small binaries, an easy `wasm32-wasi`
  target, and good protobuf tooling — but Rust, Go (TinyGo), C, etc.
  all work against the same ABI.

## How it fits together

```
┌──────────────────────────────────────────────────────────────────┐
│ OpenTelemetry Collector                                          │
│                                                                  │
│   ┌──────────────────────────┐         ┌────────────────────┐    │
│   │   wasm4otel receiver     │  OTLP   │  next consumer     │    │
│   │   (Go, this repo)        │────────▶│  (processor/export)│    │
│   │                          │         └────────────────────┘    │
│   │   ┌──────────────────┐   │                                   │
│   │   │   wazero runtime │   │                                   │
│   │   │  ┌────────────┐  │   │                                   │
│   │   │  │  .wasm     │  │   │                                   │
│   │   │  │  plugin    │  │   │                                   │
│   │   │  └────────────┘  │   │                                   │
│   │   └──────────────────┘   │                                   │
│   └──────────────────────────┘                                   │
└──────────────────────────────────────────────────────────────────┘
```

The host (Go, in `go/`) provides a small set of imports the guest can
call:

| Import       | Purpose                                                              |
| ------------ | -------------------------------------------------------------------- |
| `host_log`   | Send a log line to the collector's own logger (zap levels).          |
| `push_logs`  | Hand an OTLP-encoded `LogsData` protobuf to the next consumer.       |

And expects the guest to export `start()` / `stop()` (called on
collector startup and shutdown), plus the usual WASI `_initialize` or
freestanding `_start` entrypoint.

`push_metrics` / `push_traces` and the symmetric `consume_*` exports
(for processor/exporter-style plugins) are sketched but not wired yet.

## Repository layout

```
go/          Collector receiver. Exports NewFactory() for embedding
             in a collector distribution.
zig/         Example plugins in Zig:
  freestanding/helloworld.zig    Minimal wasm32-freestanding plugin
  wasip1/log_generator.zig       wasm32-wasi reactor plugin that emits
                                 a batch of OTLP logs on start
  src/                           Shared modules (host_log helper, OTLP
                                 protobuf re-exports)
```

## Quickstart

### Build the example plugin (Zig)

Requires Zig **0.16.0** or newer.

```sh
cd zig
zig build           # outputs zig-out/bin/log_generator.wasm and helloworld.wasm
```

The first build also fetches `zig-protobuf` and the official
`opentelemetry-proto` repo (versions pinned in `build.zig.zon`) and
generates the OTLP types into `zig/src/opentelemetry/`.

### Use it from a collector

`wasm4otel` is a Go package — to actually run it, register the factory
in an OpenTelemetry Collector distribution (e.g. via
[`ocb`](https://opentelemetry.io/docs/collector/custom-collector/)):

```go
import wasm4otel "github.com/agagniere/wasm4otel/go"

// add wasm4otel.NewFactory() to your receivers list
```

Then point a receiver entry at the built `.wasm`:

```yaml
receivers:
  wasm4otel:
    path: /path/to/log_generator.wasm

service:
  pipelines:
    logs:
      receivers: [wasm4otel]
      exporters: [debug]
```

On startup, the plugin's `start()` runs once, emits six log records
through `push_logs`, and they flow out the configured exporter.

## Writing a plugin

In any wasm-capable language:

1. Import `env.host_log(level, ptr, size)` if you want to log via the
   collector.
2. Import `env.push_logs(ptr, size) -> i32` and pass it an
   OTLP-serialized `LogsData` protobuf. Returns 0 on success.
3. Export `start()` and `stop()`.
4. Compile to `wasm32-wasi` (reactor) or `wasm32-freestanding`.

The Zig examples in `zig/freestanding/` and `zig/wasip1/` are intended
to be readable templates.

## Roadmap

- Wire `push_metrics` / `push_traces` on the host side.
- Implement the `consume_*` exports so plugins can act as processors
  and exporters, not just receivers.
- Move from the hand-rolled ABI to WIT-defined Component Model
  bindings.
- Switch wazero from interpreter mode to the optimizing compiler.
- Pass `plugin_config` from collector YAML through to the guest.

## Acknowledgements

- [wazero](https://github.com/tetratelabs/wazero) — pure-Go wasm runtime.
- [otelwasm](https://github.com/otelwasm/otelwasm) — prior art; the
  config struct in `go/config.go` is borrowed from there (Apache 2.0).
- [zig-protobuf](https://github.com/Arwalk/zig-protobuf) — protobuf
  codegen for Zig.
- [opentelemetry-proto](https://github.com/open-telemetry/opentelemetry-proto) —
  the OTLP `.proto` definitions.
