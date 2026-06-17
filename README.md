# wasm4otel

OpenTelemetry Collector components as **WebAssembly plugins**.

- **More flexible**: Plugins can be added, removed, updated, without rebuilding the collector
- **Wider ecosystem**: Written in any language that can be compiled to Wasm: C, C++, Rust, Zig, ...
- **Leaner distribution**: The collector binary can be smaller while allowing more components to be used
- **Security**: Wasm plugins can only access what the host allowed them to access

> :warning: **Status: proof of concept.** The host/guest ABI is hand-rolled.
> The **logs** signal is wired for all three roles (receiver, processor,
> exporter) on the host side; metrics and traces remain sketched but
> not wired. The Zig template tree ships a receiver example today; a
> processor/exporter template that exports `consume_logs` is the next
> Zig-side change. APIs will change.

## How it fits together

A wasm4otel component is a Go shim that owns a wazero runtime and a
single `.wasm` module. The same shim plays three different roles
depending on which factory is registered in the collector — receiver
(plugin pushes telemetry from its own loop), processor (plugin
transforms each batch handed to it), exporter (plugin terminally
consumes each batch). The diagram below shows the receiver path; the
processor and exporter paths flow `consume_logs` into the guest and
optionally back out via `push_logs`.

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

| Import                   | Purpose                                                                   |
|--------------------------|---------------------------------------------------------------------------|
| `host_log`               | Send a log line to the collector's own logger (zap levels).               |
| `push_logs`              | Hand an OTLP-encoded `LogsData` protobuf to the next consumer.            |
| `interruptible_sleep_ms` | Sleep at most N ms; returns non-zero when the component is shutting down. |

And it looks up a symmetric set of exports the host can call to
**deliver** telemetry the plugin should consume — `consume_logs`,
`consume_metrics`, `consume_traces`. A processor or exporter plugin
implements these, plus two small allocator exports `wasm4otel_alloc(size) -> ptr`
and `wasm4otel_free(ptr, size)` so the host can hand a batch into the guest's
linear memory. Lifecycle exports `start()` / `stop()` (called on
collector startup and shutdown) are optional, and the usual WASI
`_initialize` or freestanding `_start` entrypoint applies.

The logs signal is wired end-to-end today: `consume_logs` is invoked
synchronously per batch in processor/exporter mode, while receiver-mode
plugins still drive their own loop via `push_logs`. Metrics and traces
exports are looked up but not yet routed to consumers.

## Quickstart

### Build the example plugin (Zig)

Requires Zig **0.16.0** or newer.

```shell
cd zig
zig build --release    # outputs *.wasm files in zig-out/bin/
```

The first build also fetches `zig-protobuf` and the official
`opentelemetry-proto` repo (versions pinned in `build.zig.zon`) and
generates the OTLP types into `zig/src/opentelemetry/`.

### Use it from a collector

`wasm4otel` is a Go module — to actually run it, register the
factory of whichever role you need in an OpenTelemetry Collector
distribution (e.g. via
[`ocb`](https://opentelemetry.io/docs/collector/custom-collector/)):

```go
import (
    wasm4otelreceiver  "github.com/agagniere/wasm4otel/go/receiver"
    wasm4otelprocessor "github.com/agagniere/wasm4otel/go/processor"
    wasm4otelexporter  "github.com/agagniere/wasm4otel/go/exporter"
)

// add the factories your distribution needs
wasm4otelreceiver.NewFactory()
wasm4otelprocessor.NewFactory()
wasm4otelexporter.NewFactory()
```

Each factory uses the type name `wasm4otel`; the YAML disambiguates
them by which pipeline section the entry appears under:

```yaml
receivers:
  wasm4otel:
    path: /path/to/log_generator.wasm

processors:
  wasm4otel:
    path: /path/to/severity_filter.wasm

exporters:
  debug:

service:
  pipelines:
    logs:
      receivers:  [wasm4otel]
      processors: [wasm4otel]
      exporters:  [debug]
```

On startup, the receiver plugin's `start()` runs on its own goroutine
and pushes batches via `push_logs` until shutdown. Each batch flows
into the processor plugin's `consume_logs`, which transforms the batch
and pushes the result out via its own `push_logs`. The configured
exporter then writes it to its sink.

## Writing a plugin

In any wasm-capable language:

1. Import `env.host_log(level, ptr, size)` if you want to log via the
   collector.
2. Decide which role(s) you want the plugin to fill — receiver,
   processor, or exporter. The plugin's export table tells the host
   what it can do; the YAML pipeline entry decides what it is.
3. **For a receiver:** import `env.push_logs(ptr, size) -> i32` and
   pass it an OTLP-serialized `LogsData` protobuf. Export `start()` to
   drive your loop (and `stop()` if you need a graceful tear-down hook).
4. **For a processor or exporter:** export `consume_logs(ptr, size) -> i32`
   so the host can deliver each batch, plus the two allocator exports
   `wasm4otel_alloc(size) -> ptr` and `wasm4otel_free(ptr, size)` so the host can write
   into your linear memory. A processor that wants to forward its
   transformed batch downstream also imports `push_logs` and calls it
   inside `consume_logs`.
5. Compile to `wasm32-wasi` (reactor) or `wasm32-freestanding`.

The Zig examples in `zig/freestanding/` and `zig/wasip1/` are intended
to be readable templates. They currently cover the receiver path; a
processor/exporter template is the next Zig-side change.

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

- Extend processor/exporter wiring to `consume_metrics` /
  `consume_traces` and add the matching `push_metrics` / `push_traces`
  host imports.
- Ship a Zig processor template that exports `consume_logs`,
  `wasm4otel_alloc`, `wasm4otel_free`, and re-publishes filtered /
  enriched batches via `push_logs`.
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
