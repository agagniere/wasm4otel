# wasm4otel

OpenTelemetry Collector components as **WebAssembly plugins**.

- **More flexible**: Plugins can be added, removed, updated, without rebuilding the collector
- **Wider ecosystem**: Written in any language that can be compiled to Wasm: C, C++, Rust, Zig, ...
- **Leaner distribution**: The collector binary can be smaller while allowing more components to be used
- **Security**: Wasm plugins can only access what the host allowed them to access

> :warning: **Status: proof of concept.** The host/guest ABI is hand-rolled.
> Today, only the **logs receiver** path is wired end-to-end; metrics,
> traces, and the processor/exporter direction are sketched in the
> host code but not yet functional. APIs will change.

## How it fits together

A wasm4otel component is a Go shim that owns a wazero runtime and a
single `.wasm` module. For now it can only be a receiver:

```
┌──────────────────────────────────────────────────────────────────┐
│ OpenTelemetry Collector                                          │
│                                                                  │
│   ┌──────────────────────────┐         ┌────────────────────┐    │
│   │   wasm4otel component    │  OTLP   │  next consumer     │    │
│   │   (Go, this repo)        │────────▶│                    │    │
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

The host (Go, in `go/`) exposes a small set of imports the guest can
call to **push** telemetry into the next consumer:

| Import       | Purpose                                                              |
| ------------ | -------------------------------------------------------------------- |
| `host_log`   | Send a log line to the collector's own logger (zap levels).          |
| `push_logs`  | Hand an OTLP-encoded `LogsData` protobuf to the next consumer.       |

And it looks up a symmetric set of exports the host can call to
**deliver** telemetry the plugin should consume — `consume_logs`,
`consume_metrics`, `consume_traces`. These are what a processor or
exporter plugin would implement. Plus lifecycle exports `start()` /
`stop()` (called on collector startup and shutdown) and the usual
WASI `_initialize` or freestanding `_start` entrypoint.

Today only `host_log` and `push_logs` are wired end-to-end. The
remaining imports and exports are reserved slots that the next round
of work will fill in.

## Quickstart

### Build the example plugin (Zig)

Requires Zig **0.16.0** or newer.

```shell
cd zig
zig build     # outputs *.wasm files in zig-out/bin/
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

## Reference docs

For a refresher on the underlying tech:

- [`WASM.md`](WASM.md) — WebAssembly spec versions, the proposal
  lifecycle, and a master table of proposals with Wasmtime tier.
- [`WASI.md`](WASI.md) — WASI revisions (Preview 0 → 3), the
  component-model worlds, and how to pick a target.

For what each side of *this* project supports:

- [`go/README.md`](go/README.md) — host package: public API, config,
  lifecycle, host imports / guest exports.
- [`go/WAZERO.md`](go/WAZERO.md) — wazero's feature matrix and what
  it means for plugins this collector can load.
- [`zig/README.md`](zig/README.md) — Zig build system, shared
  modules, conventions for adding a plugin.
- [`zig/WASM.md`](zig/WASM.md) — Zig's wasm targets, feature flags,
  CPU models, and cross-check against wazero.

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
