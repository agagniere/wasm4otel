# `rust/` — embeddable wasmtime host

A Rust library that runs wasm4otel plugins on
[wasmtime](https://wasmtime.dev). It speaks the same core-wasm ABI v2 as
the two Go hosts, so the same `.wasm` runs under all three, and it is
checked against the same conformance fixtures in `go/testdata/`.

Unlike the Go hosts, it is not a collector component. It owns the wasm
side and moves OTLP protobuf bytes in and out; the embedding application
wires those bytes into its own pipeline. The first intended embedder is
[Saluki](https://github.com/DataDog/saluki), where a receiver plugin maps
to a relay, an exporter plugin to a forwarder, and a processor plugin to a
component that takes raw OTLP payloads in and out.

> [!WARNING]
> Proof of concept, like the rest of the repo. The API will change.

## Using it

```rust
use wasm4otel::{Options, Runtime, RuntimeMode, Signal};

// Once per process.
let runtime = Runtime::new(RuntimeMode::Auto)?;

// Once per plugin file: compiles, and resolves the imports.
let plugin = runtime.load("severity_filter.wasm")?;

// Once per role the plugin is wired as: a new instance each time.
let mut processor = plugin.processor(Options { plugin_config: Some(r#"{"min":"warn"}"#.into()) }).await?;
processor.supports(Signal::Logs)?;
processor.start().await?;

match processor.process(Signal::Logs, &otlp_logs_data).await? {
    Some(output) => { /* hand `output` downstream */ }
    None => { /* the plugin filtered the whole batch out */ }
}

processor.shutdown().await;
```

The three role types:

| Type        | Built by                         | Per batch                    | Pushes go to                  |
|-------------|----------------------------------|------------------------------|-------------------------------|
| `Receiver`  | `plugin.receiver(opts, consumer)`| — (`run_until(shutdown)`)    | the `Consumer` you pass       |
| `Processor` | `plugin.processor(opts)`         | `process(signal, bytes)`     | the return value of `process` |
| `Exporter`  | `plugin.exporter(opts)`          | `export(signal, bytes)`      | nowhere (rc 3)                |

Instantiating a role runs the same checks as the Go hosts' `Load`:
`wasm4otel_receive` for a receiver, `wasm4otel_alloc`/`wasm4otel_free`
for the other two, then `wasm4otel_setup`. `supports(signal)` is the
per-signal `Validate<Signal>Export`. Error messages use the Go hosts'
wording.

## One engine

wazero gave each Go `Component` its own runtime. Here a `Runtime` holds
**one** `wasmtime::Engine` and one import linker for the whole process:

- the `env` imports and WASIp1 are defined once, in a linker shared by
  every instance — the host functions keep no state of their own, they
  read the calling instance's store;
- `Runtime::load` compiles a plugin once and pre-resolves its imports
  (`InstancePre`), so an unknown import fails at load time and each role
  instantiation is just memory setup;
- every instance gets its own `Store` and linear memory, so one `.wasm`
  wired as a receiver, a processor and an exporter is compiled once and
  still runs as three isolated instances (see the
  `one_compile_serves_every_role` test).

The cost is that the backend is engine-wide: `RuntimeMode`
(`auto` | `interpreter` | `compiled`, same spellings and default as the Go
`runtime.mode`) is chosen once per `Runtime`, not per plugin.
`interpreter` is wasmtime's Pulley; `compiled` returns an error, rather
than panicking, on a CPU without a Cranelift backend.

## Where it differs from the Go hosts

- **Async.** Every guest call is a future. The guest runs on a wasmtime
  fiber, so `push_<signal>` in a receiver awaits the `Consumer` — that is
  the backpressure path — and `interruptible_sleep_ms` is a Tokio timer
  raced against shutdown. The receive loop is an ordinary task, not a
  dedicated thread. A Tokio runtime is required.
- **No mutex.** Every guest entry takes `&mut self`, so the borrow checker
  gives the single-occupancy guarantee that `callMu` enforces at run time.
  The poison latch after a trap is the same.
- **No protobuf decoding.** The host never decodes OTLP. A push is only
  checked to be well-formed protobuf at the top level, which is what
  `push_<signal>` returning 2 means here; the Go hosts run a full
  `ProtoUnmarshaler`. A processor's pushes are concatenated, which for
  these messages is the byte-level equivalent of `MoveAndAppendTo`.
- **WASI sleep is real.** wasmtime-wasi implements `poll_oneoff` clock
  waits, where wazero without `WithSysNanosleep` returns immediately. It
  is still not interruptible: plugins should keep pacing through
  `interruptible_sleep_ms`.
- **Guest logs go to `tracing`,** tagged with a `plugin` field. zap levels
  map to `debug`/`info`/`warn`/`error`; the panic and fatal levels are
  logged as errors and never abort the process.
- **The plugin must export its memory as `memory`,** which every Zig and
  WAT plugin here already does.

## Not done yet

- No epoch or fuel limits: a guest that loops without calling the host
  keeps the thread polling it busy.
- No compiled-module cache on disk (`Module::serialize`).
- The Component Model / WIT path (`interface/wasm4otel.wit`) — wasmtime's
  `bindgen!` can generate the host side, which no Go runtime offers.

## Building and testing

```sh
cargo test
cargo clippy --all-targets -- -D warnings
```

or `make test-rust` from the repo root. The conformance tests read the
fixtures from `../go/testdata/`, so run them from a full checkout.
