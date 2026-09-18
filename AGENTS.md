# AGENTS

This file provides guidance to LLMs when working with code in this repository.

## What this project is

`wasm4otel` is a set of OpenTelemetry Collector components that run WebAssembly plugins. The host (Go) embeds [wazero](https://github.com/tetratelabs/wazero) and instantiates a guest `.wasm` plugin; the guest can act as a receiver (drives its own loop, pushes telemetry), a processor (transforms each batch handed to it), or an exporter (terminally consumes each batch).

The repo has two top-level pieces:

- `go/` — the host. Single Go module (`github.com/agagniere/wasm4otel/go`) with four packages: a shared `wasm4otel` package holding the `Component` type and host imports, plus three role packages (`go/receiver`, `go/processor`, `go/exporter`) each exposing `NewFactory()`.
- `zig/` — example guest plugins built in Zig (three freestanding, two WASI preview1 reactors) plus shared modules.

## Host/guest ABI

The contract between Go and the guest is defined imperatively in `go/component.go`. Keep both sides in sync when changing it.

Host imports exposed in the `env` module (called from the guest):
- `host_log(level: i32, ptr, size)` — level matches `go.uber.org/zap/zapcore.Level` (debug=-1, info=0, …). The Zig side mirrors this in `zig/src/log.zig`'s `LogLevel` enum.
- `push_logs(ptr, size) -> i32` / `push_metrics(ptr, size) -> i32` / `push_traces(ptr, size) -> i32` — bytes are an OTLP-encoded `LogsData` / `MetricsData` / `TracesData` protobuf. Returns 0 on success, non-zero error code otherwise (1 = bad memory read, 2 = decode failure, 3 = no downstream consumer, 4 = downstream consumer rejected the batch). A processor plugin calls these from inside its `wasm4otel_process_<signal>` to forward the transformed batch; an exporter doesn't call them (or calls them knowing the host returns 3). A receiver calls them from its `wasm4otel_receive` loop.
- `interruptible_sleep_ms(ms: u32) -> u32` — sleeps for up to `ms` milliseconds. Returns 0 when the duration elapsed, non-zero when the component's context fires (Shutdown). The Zig wrapper in `zig/src/host.zig` (`host` module) surfaces this as `interruptibleSleep(std.Io.Duration) error{Interrupted}!void`. Works in both freestanding and wasip1 — does not depend on any WASI plumbing.
- `get_config(ptr: u32, size: u32) -> u32` — hands the YAML `plugin_config` map to the guest as a JSON document. Returns the document's true byte length. If `size >= true_size`, writes the bytes at `ptr`; if `size < true_size`, writes nothing so the guest can re-allocate and call again. Returns 0 when YAML didn't set `plugin_config`. Probe with `(0, 0)` to learn the size without touching memory. The Zig wrapper `host.getConfigAlloc(allocator)` does the probe-then-read dance and returns `std.mem.Allocator.Error!?[]u8` (caller frees; `null` means no `plugin_config`).

Guest exports the host calls. **Every name this ABI defines carries the `wasm4otel_` prefix** (the WASI entrypoints below are the exception — they aren't ours), so a module's export table is self-evidently its ABI surface — and so `shutdown` doesn't collide with the POSIX socket call wasi-libc defines. Zig plugins put the whole surface in one `comptime` block of `@export(&fn, .{ .name = "wasm4otel_…" })` calls, which keeps the wire names prefixed while the Zig functions behind them aren't.

Lifecycle, in call order:
- `wasm4otel_setup() -> i32` — invoked from `Load`, inside the factory's `createX`. The plugin's chance to read `plugin_config` via `get_config` and refuse it: `0 = success`, `1 = generic failure`, `2 = invalid user-provided config`. Non-zero becomes an error out of `createX`, so a bad YAML config stops the collector from finishing its boot rather than failing at the first batch. Zig side: `guest.SetupResult`.
- `wasm4otel_start() -> i32` — invoked from `Start(ctx, host)`, synchronously, in **every** mode. Short-lived init that didn't fit in setup (opening a file now that config is known good); it must return promptly so the pipeline can begin work. `0 = success`, `1 = generic failure`. Zig side: `guest.StartResult`.
- `wasm4otel_receive() -> i32` — **receiver mode only**, spawned on its own goroutine by `Start` once `wasm4otel_start` returns success. The long-running loop that pushes telemetry; it's expected to block until Shutdown cancels the component context, which surfaces to the guest as a non-zero `interruptible_sleep_ms` return. Receivers are signal-agnostic here — one loop calls whichever `push_<signal>` it needs. Same rc enum as start.
- `wasm4otel_shutdown()` — invoked from `Shutdown(ctx)`. The host cancels the context first, then waits for the receive goroutine to drain (a no-op in processor/exporter mode), then calls this.
- `_initialize` (WASI reactor) or `_start` (command / freestanding) — unprefixed because they aren't ours; the plugin declares one or the other, as wasm-ld requires for the `wasi_exec_model` selected. `LoadPlugin` passes both names to `ModuleConfig.WithStartFunctions(...)` and wazero calls whichever is present.

Guests should `host_log` their own detail before returning non-zero from any of these. Empty results (`() -> ()`) are accepted as success for forward-compat, but the convention is the i32 return so the plugin can signal failure. Every lifecycle export is optional, except `wasm4otel_receive` in receiver mode — a processor or exporter that exports none of them still loads, it just does nothing on those events.

Batch handling, role-typed as well as signal-typed:
- `wasm4otel_process_logs(ptr, size) -> i32` / `_metrics` / `_traces` — required for **processor** mode on the matching signal. A processor forwards its result through the `push_<signal>` host import, so `process_` is the half that hands work on.
- `wasm4otel_export_logs(ptr, size) -> i32` / `_metrics` / `_traces` — required for **exporter** mode on the matching signal. An exporter is terminal.
- The host invokes exactly one of these per incoming batch with the host-allocated buffer pointer and size; 0 on success, non-zero for plugin-side errors. The split is what lets the export table alone say which role a plugin was written for — no env var, no runtime declaration. A plugin can export both names, but forwarding is precisely what the two halves don't share, so they generally need two bodies: `freestanding/helloworld.zig` exports all six, its `process_` half passing the host's buffer straight back through `push_<signal>` and its `export_` half dropping it. That pass-through is also the repo's benchmark baseline — the batch never gets decoded, so what it costs is what the wasm hop costs.
- `wasm4otel_alloc(size: u32) -> u32`, `wasm4otel_free(ptr: u32, size: u32)` — required alongside the batch exports for processor/exporter mode. The host calls `wasm4otel_alloc` to reserve a region in the guest's linear memory, writes the OTLP payload into it, invokes the batch export, then calls `wasm4otel_free`. Returning 0 from `wasm4otel_alloc` signals failure; the host treats 0 as a non-trapping skip and does not call `wasm4otel_free`. The prefix matters most visibly on these two: Rust + wasm32-wasi links wasi-libc, which already defines `free`, so an unprefixed export would collide at link time.

The role each plugin plays is decided by which factory the operator registers in the OTel collector YAML — `receivers:`, `processors:`, or `exporters:` — not by the plugin itself. Validation is **additive**: each check asks whether the plugin exports what *this* role needs, never whether it also exports something for another role. A single `.wasm` can therefore be deployed in all three sections at once, each wiring getting its own `Component` instance playing exactly one role. Two layers, in order:

1. `Load` runs `validateModeExports` — receiver mode requires `wasm4otel_receive`; processor and exporter modes require the `wasm4otel_alloc` / `wasm4otel_free` pair — then `runSetup`.
2. The factory's `createX` then calls the per-signal `Validate<Signal>Export`, which requires `wasm4otel_process_<signal>` (processor) or `wasm4otel_export_<signal>` (exporter) for the pipeline section the plugin sits under.

Note the ordering: `wasm4otel_setup` runs *before* the per-signal check, so a plugin's setup hook can run even when the signal wiring is about to be rejected. Error messages report what's missing for the role the operator wired, never what's present but incompatible — the goal is to guide them to either fix the YAML or rebuild the plugin with the right export.

## Building and running

### Zig guests (canonical build)

From `zig/`:

```sh
zig build                # builds all freestanding + wasip1 plugins into zig/zig-out/bin/
zig build gen-proto      # regenerate src/opentelemetry/proto/**/*.pb.zig from otelproto dep
```

`build.zig` declares two target sets driven by the `freestanding_sources` / `wasip1_sources` arrays at the bottom — add a new plugin by appending to the right list with its filename and the symbols to export. `build.zig` only selects the execution model (`wasi_exec_model = .reactor` for the wasip1 set, none for freestanding) — it does not emit an entry symbol. The plugin source declares `_initialize` (reactor) or `_start` (freestanding) itself; see *Things that are easy to get wrong* below.

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
- Wazero is deterministic by default: without explicit opt-ins a guest sees a frozen `2022-01-01T00:00:00Z` clock and a seeded `random_get`. `LoadPlugin` wires `WithSysWalltime`, `WithSysNanotime` and `WithRandSource(crypto/rand.Reader)`, so plugins get real time and real entropy — but *not* `WithSysNanosleep`, because pacing goes through the `interruptible_sleep_ms` host import instead. Don't add `WithSysNanosleep` to "fix" a plugin whose WASI sleep returns immediately; move the plugin to `interruptibleSleep`. `go/WAZERO.md` has the full table.
- Processor/exporter plugins are single-occupant — `Component.callMu` serializes every `Call()` into the wasm instance, because wazero modules are not safe for concurrent calls. The receiver-mode `wasm4otel_receive` goroutine runs without the mutex (it owns the instance for its lifetime), which is why one `Component` plays one role even when the plugin exports enough to play several. The single-occupancy rule binds per instance, not per export set, so multi-role plugins are still safe — they just get an instance per YAML section.
- After any guest trap (`wasm4otel_setup`, `wasm4otel_start`, `wasm4otel_receive`, `wasm4otel_alloc`, a batch export, `wasm4otel_free`, `wasm4otel_shutdown`), `Component.broken` latches and further guest entries fail closed. The collector will see a stream of errors; we don't try to recover the instance. `receiveLoop` latches it by hand, since it runs outside `invoke()`.
