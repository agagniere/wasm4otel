# `freestanding/` — `wasm32-freestanding` plugins

Plugins built for the `wasm32-freestanding` target, i.e. a WebAssembly
module with **no WASI imports**. The host loads them the same way as
WASIp1 plugins, but the wasm side has no operating system to call into.

See [`../README.md`](../README.md) for the build system and shared
modules. This document covers the freestanding target in particular.

## When to use this target

The interest of the freestanding target in the context of OpenTelemetry
collector components is quite limited: no system calls means no access
to external ressources like files, network, clocks.

Only pure processors could realistically want to use this target.

Currently it is only used for learning purposes and as a way to better
illustrate the benefits of WASI in contrast.

Two examples live here:

- `helloworld.zig` — logs through `host_log` and nothing else.
  Release build: **~1.5 KB**.
- `one_log.zig` — builds a one-record `LogsData`, encodes it to OTLP
  protobuf via `otel_pipeline_data`, and pushes it through
  `push_logs`. Release build: **~11 KB**.

`one_log.zig` answers the obvious question: **OTLP encoding is fully
freestanding-compatible.** The protobuf encoder is pure byte-pushing
into a writer, the generated structs touch no syscalls, and
`std.heap.wasm_allocator` works without WASI. Inspecting the output
with `wasm-tools print` shows only two imports —
`(import "env" "host_log" ...)` and `(import "env" "push_logs" ...)`
— and zero `wasi_snapshot_preview1` references.

## What works

- **Host imports** declared as `extern fn` — they resolve against the
  `env` module the host registers.
- **Exports** via `@export` or the `export` keyword — names listed in
  `OtelPlugin.symbols` are made visible.
- **Memory and allocation.** `std.heap.wasm_allocator` works; it grows
  the module's linear memory via `@wasmMemoryGrow` without touching
  any OS API.
- **Most of `std`** that is pure logic: `std.fmt`, `std.mem`,
  `std.ArrayList`, `std.json`, `std.hash`, `std.sort`, the
  protobuf-shaped types from `otel_pipeline_data`, etc.
- **`std.log`** using `host.logFn`

## What doesn't work

Anything that would normally go through WASI is unavailable:

| Feature                              | Why it fails on freestanding                              |
| ------------------------------------ | --------------------------------------------------------- |
| `std.fs.*` (files, dirs)             | needs `fd_*` from `wasi_snapshot_preview1`                |
| `std.Io.Clock.now(.real, io)` etc.   | needs `clock_time_get`                                    |
| `std.crypto.random` via the OS path  | needs `random_get`                                        |
| Sleep via `Io.Threaded.io().sleep(...)` | needs `poll_oneoff` — but `host.interruptibleSleep` from the `host` module works here |
| `std.process.argsAlloc`, env vars    | needs `args_get` / `environ_get`                          |
| Writing to stdout / stderr           | needs `fd_write` on fd 1/2                                |
| `std.process.exit(n)`                | needs `proc_exit` — `unreachable` traps instead           |

If a plugin reaches into one of these, the failure is at *compile or
link time*, not runtime — wasm-ld will report an undefined import for
the missing WASI symbol. That is the signal to either move the plugin
to [`../wasip1/`](../wasip1/) or to expose what it needs as a new host
import on the Go side.

The constraint shows up in `one_log.zig`: with no real-time clock
available, `time_unix_nano` and `observed_time_unix_nano` are left at
their default of `0`. A freestanding plugin that needs an honest
timestamp has to either receive it from the host (e.g. as an extra
parameter to `start`, or via a new `host_now()` import) or move to
the WASIp1 target.

Pacing is *not* on this list of constraints — `host.interruptibleSleep`
is a host import, not WASI plumbing, so freestanding plugins can loop
and pace exactly like wasip1 plugins do (and unwind on shutdown the
same way). What still belongs in [`../wasip1/`](../wasip1/) are uses
of `poll_oneoff` proper: multiplexing readiness across file
descriptors, signalfd-style waits, etc.

## Entrypoint

`wasm-ld` defaults to requiring `_start` as the module's entry
symbol. The freestanding target inherits that default, so a plugin
that does not export `_start` fails to link with:

```
wasm-ld: entry symbol not defined (pass --no-entry to suppress): _start
```

This also lines up with what `LoadPlugin` calls on the host side —
`WithStartFunctions("_start", "_initialize")` — so the export is
auto-invoked on instantiation.

To use a different name (or no entry at all), the build script would
need to pass `--no-entry` / `-fentry=<name>` to the linker.

## Adding a freestanding plugin

Append an entry to `freestanding_sources` in
[`../build.zig`](../build.zig):

```zig
const freestanding_sources: []const OtelPlugin = &.{
    .{ .filename = "helloworld.zig", .symbols = &.{ "start", "stop" } },
    // .{ .filename = "my_plugin.zig", .symbols = &.{ "start", "stop" } },
};
```

The `host`, `guest`, and `otel_pipeline_data` modules are wired into
the freestanding loop by default — see `build.zig`.
