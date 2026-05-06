# `freestanding/` — `wasm32-freestanding` plugins

Plugins built for the `wasm32-freestanding` target, i.e. a WebAssembly
module with **no WASI imports**. The host loads them the same way as
WASIp1 plugins, but the wasm side has no operating system to call into.

See [`../README.md`](../README.md) for the build system and shared
modules. This document covers what the freestanding target buys and
what it costs.

## When to use this target

Pick `freestanding` when the plugin's work is pure computation over
the bytes the host hands in, and the only outside-world interaction it
needs goes through the host imports declared in the `env` module
(`host_log`, `push_logs`, etc.).

Reasons to prefer it over `wasip1`:

- **Smaller binaries.** No WASI runtime, no libc-shaped glue.
- **No determinism gotchas.** Without clocks/random, there is nothing
  for wazero to default to a fixed value (see
  [`../../go/WAZERO.md`](../../go/WAZERO.md) — defaults that bite
  WASIp1 don't apply here).
- **Simpler ABI surface.** The only imports are the ones explicitly
  declared with `extern fn`.

`helloworld.zig` is the canonical example: it logs through `host_log`
and does nothing else.

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
- **`std.log`** — provided you wire a `logFn` that doesn't try to
  reach stderr. The `hostlog` module does exactly this.

## What doesn't work

Anything that would normally go through WASI is unavailable:

| Feature                              | Why it fails on freestanding                              |
| ------------------------------------ | --------------------------------------------------------- |
| `std.fs.*` (files, dirs)             | needs `fd_*` from `wasi_snapshot_preview1`                |
| `std.Io.Clock.now(.real, io)` etc.   | needs `clock_time_get`                                    |
| `std.crypto.random` via the OS path  | needs `random_get`                                        |
| Sleep / `Io.Threaded.io().sleep(...)`| needs `poll_oneoff`                                       |
| `std.process.argsAlloc`, env vars    | needs `args_get` / `environ_get`                          |
| Writing to stdout / stderr           | needs `fd_write` on fd 1/2                                |
| `std.process.exit(n)`                | needs `proc_exit` — `unreachable` traps instead           |

If a plugin reaches into one of these, the failure is at *compile or
link time*, not runtime — wasm-ld will report an undefined import for
the missing WASI symbol. That is the signal to either move the plugin
to [`../wasip1/`](../wasip1/) or to expose what it needs as a new host
import on the Go side.

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

The `hostlog` module is wired by default; `otel_pipeline_data` is not.
