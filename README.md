# wasm4otel

OpenTelemetry Collector components as **WebAssembly plugins**.

- **More flexible**: Plugins can be added, removed, updated, without rebuilding the collector
- **Wider ecosystem**: Written in any language that can be compiled to Wasm: C, C++, Rust, Zig, ...
- **Leaner distribution**: The collector binary can be smaller while allowing more components to be used
- **Security**: Wasm plugins can only access what the host allowed them to access

> :warning: **Status: proof of concept.** The host/guest ABI is
> hand-rolled: every guest export the host calls carries a
> `wasm4otel_` prefix, and the batch exports are role-typed —
> `wasm4otel_process_<signal>` for a processor,
> `wasm4otel_export_<signal>` for an exporter. All three signals
> (logs, metrics, traces) are wired end-to-end for all three roles
> (receiver, processor, exporter). The Zig template tree covers a logs
> receiver (`wasip1/log_generator.zig`), two logs processors
> (`wasip1/severity_parser.zig`, `freestanding/severity_filter.zig`),
> and a whole-surface template that forwards batches untouched
> (`freestanding/helloworld.zig`); metrics and traces flow end-to-end
> but no Zig template decodes them yet.
> APIs will change.

## How it fits together

A wasm4otel component is a Go shim that owns a wazero runtime and a
single `.wasm` module. The same shim plays three different roles
depending on which factory is registered in the collector — receiver
(plugin pushes telemetry from its own loop), processor (plugin
transforms each batch handed to it), exporter (plugin terminally
consumes each batch). The diagram below shows the receiver path; the
processor and exporter paths flow `wasm4otel_process_<signal>` or
`wasm4otel_export_<signal>` into the guest and, for a processor, back
out via `push_<signal>` (one set per signal: logs, metrics, traces).

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
| `push_metrics`           | Hand an OTLP-encoded `MetricsData` protobuf to the next consumer.         |
| `push_traces`            | Hand an OTLP-encoded `TracesData` protobuf to the next consumer.          |
| `interruptible_sleep_ms` | Sleep at most N ms; returns non-zero when the component is shutting down. |
| `get_config`             | Fetch the YAML `plugin_config` as a JSON document.                        |

And it looks up a symmetric set of exports the host can call to
**deliver** telemetry the plugin should consume. These are role-typed
as well as signal-typed — a processor exports
`wasm4otel_process_<signal>`, an exporter `wasm4otel_export_<signal>`
— plus two small allocator exports, `wasm4otel_alloc(size) -> ptr`
and `wasm4otel_free(ptr, size)`, so the host can hand a batch into
the guest's linear memory.

| Export                        | Called                                                | Role          |
|-------------------------------|-------------------------------------------------------|---------------|
| `wasm4otel_setup`             | In the factory, to validate `plugin_config`           | All           |
| `wasm4otel_start`             | On collector startup, synchronously, must return fast | All           |
| `wasm4otel_receive`           | On collector startup, on its own goroutine            | Receiver only |
| `wasm4otel_shutdown`          | On collector shutdown                                 | All           |
| `wasm4otel_process_<signal>`  | Once per incoming batch                               | Processor     |
| `wasm4otel_export_<signal>`   | Once per incoming batch                               | Exporter      |
| `wasm4otel_alloc` / `_free`   | Around each batch                                     | Processor, exporter |

Only two things are mandatory: `wasm4otel_receive` in receiver mode,
and the matching batch export plus the allocator pair in processor and
exporter mode. Everything else is optional. The usual WASI
`_initialize` or freestanding `_start` entrypoint applies and keeps
its unprefixed name, since it isn't ours.

Because the names are role-typed, **the export table alone says which
role a plugin was written for** — no env var, no runtime declaration.
Validation is additive: each check asks only whether the plugin
exports what the role it was wired as needs. So one `.wasm` can be
deployed in all three pipeline sections at once, each wiring getting
its own component instance playing exactly one role. A plugin that
lacks the export for the section it lives under is rejected at
create-time with an error naming the export it's missing.

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

[`go/README.md`](go/README.md#adding-the-components-to-an-ocb-manifest)
has a builder manifest wiring all three, and the two fields you have
to spell out because they share one Go module.

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

On startup, the receiver plugin's `wasm4otel_receive` runs on its own
goroutine and pushes batches via `push_logs` until shutdown. Each
batch flows into the processor plugin's `wasm4otel_process_logs`,
which transforms the batch and pushes the result out via its own
`push_logs`. The configured exporter then writes it to its sink.

## Writing a plugin

In any wasm-capable language:

1. Import `env.host_log(level, ptr, size)` if you want to log via the
   collector.
2. Decide which role(s) you want the plugin to fill — receiver,
   processor, or exporter. The plugin's export table tells the host
   what it can do; the YAML pipeline entry decides what it is.
3. **For a receiver:** import `env.push_logs(ptr, size) -> i32` and
   pass it an OTLP-serialized `LogsData` protobuf. Export
   `wasm4otel_receive() -> i32` to drive your loop — the host runs it
   on its own goroutine and expects it to block until shutdown
   cancels it. Add `wasm4otel_shutdown()` if you need a graceful
   tear-down hook.
4. **For a processor or exporter:** export
   `wasm4otel_process_logs(ptr, size) -> i32` (processor) or
   `wasm4otel_export_logs(ptr, size) -> i32` (exporter) so the host
   can deliver each batch, plus the two allocator exports
   `wasm4otel_alloc(size) -> ptr` and `wasm4otel_free(ptr, size)` so
   the host can write into your linear memory. A processor forwards
   its transformed batch downstream by importing `push_logs` and
   calling it before returning — that's the difference between the two
   halves, and the reason a plugin that exports both names generally
   needs two different bodies behind them.
5. **Optionally**, export `wasm4otel_setup() -> i32` to read
   `plugin_config` via `get_config` and reject bad YAML while the
   collector is still booting, and `wasm4otel_start() -> i32` for
   short-lived init in any role.
6. Compile to `wasm32-wasi` (reactor) or `wasm32-freestanding`.

The Zig examples in `zig/freestanding/` and `zig/wasip1/` are intended
to be readable templates — `wasip1/log_generator.zig` covers the
receiver path; `freestanding/severity_filter.zig` and
`wasip1/severity_parser.zig` cover the processor path including the
`wasm4otel_alloc` / `wasm4otel_free` exports; and
`freestanding/helloworld.zig` exports the entire surface, which
makes it the shortest answer to "what are all the names?" and, because
its batch paths move bytes without decoding them, a baseline for how
much of a plugin's cost is the wasm hop itself.

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

- Move from the hand-rolled ABI to WIT-defined Component Model
  bindings.
- Use [arcjet/gravity](https://github.com/arcjet/gravity) to load
  WASI Components on wazero by transpiling them to wasip1 plugins,
  unblocking the Component Model migration before wazero supports
  components natively.
- Benchmark the wazero backends and, if the compiler wins by enough,
  make `engine: auto` the default instead of `interpreter`. The knob
  itself already exists.
- Share a single wazero runtime across all `Component` instances
  instead of one runtime per plugin.

## Acknowledgements

- [wazero](https://github.com/tetratelabs/wazero) — pure-Go wasm runtime.
- [otelwasm](https://github.com/otelwasm/otelwasm) — prior art; the
  config struct in `go/config.go` is borrowed from there (Apache 2.0).
- [zig-protobuf](https://github.com/Arwalk/zig-protobuf) — protobuf
  codegen for Zig.
- [opentelemetry-proto](https://github.com/open-telemetry/opentelemetry-proto) —
  the OTLP `.proto` definitions.
