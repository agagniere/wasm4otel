# `freestanding/` — `wasm32-freestanding` plugins

Plugins built for the `wasm32-freestanding` target, i.e. a WebAssembly
module with **no WASI imports**. The host loads them the same way as
WASIp1 plugins, but the wasm side has no operating system to call into.

See [`../README.md`](../README.md) for the build system and shared
modules. This document covers the freestanding target in particular.

## When to use this target

Everything the host offers through its `env` module works here:
`host_log`, `push_logs` / `push_metrics` / `push_traces`,
`interruptible_sleep_ms` and `get_config` are plain wasm imports, not
WASI calls. A freestanding plugin can therefore log, receive batches,
forward them, pace a loop and read its own config — and the entire
guest ABI is reachable, which `helloworld.zig` demonstrates by
exporting all twelve names.

What's unavailable is everything that would go through WASI: no
clocks, no files, no network, no entropy, no stdout. That is what
decides which roles fit.

- **Processors** fit best. The batch arrives in linear memory and
  leaves through a host import, so a transform that needs nothing but
  the bytes it was handed — filter, rewrite, enrich from config — is
  fully served. `severity_filter.zig` is the shape.
- **Receivers** work, but only for telemetry the plugin can produce
  without a clock or entropy: no real `time_unix_nano`, no generated
  trace or span IDs. `one_log.zig` shows the constraint rather than
  working around it. A receiver that polls anything outside the module
  belongs in [`../wasip1/`](../wasip1/).
- **Exporters** are the weakest fit — terminal by definition, but with
  no network and no filesystem there is nowhere for the batch to go
  except back out through `host_log`.

The upside is a module with nothing linked in to support a syscall
layer it never uses, and none of the determinism caveats WASI brings
under wazero (see [`../wasip1/README.md`](../wasip1/README.md)) —
there is no frozen clock to be surprised by if you never call one.

Three examples live here:

- `helloworld.zig` — the ABI surface itself. It exports every name the
  host looks up — `wasm4otel_setup`, `wasm4otel_start`,
  `wasm4otel_receive`, `wasm4otel_shutdown`, all six
  `wasm4otel_process_<signal>` / `wasm4otel_export_<signal>`
  variants, and the `wasm4otel_alloc` / `wasm4otel_free` pair — which
  makes it loadable in every role and the shortest reference for the
  names. (Note `wasm4otel_start`, the lifecycle hook, is a different
  export from `_start`, the entry symbol discussed below; this module
  has both.) The lifecycle hooks log and return; the batch paths do
  the least the ABI allows, which is what makes the module a
  measuring stick: `wasm4otel_process_<signal>` hands the host's
  buffer straight back through `push_<signal>` without decoding it,
  so wiring it as a processor times the wasm hop and nothing else,
  and `wasm4otel_export_<signal>` drops the batch, timing the inbound
  half alone. Both count batches and bytes and report the totals from
  `wasm4otel_shutdown` — a `host_log` per batch would cost more than
  the hop being measured. Release build: **~3.7 KB**.
- `one_log.zig` — a receiver: builds a one-record `LogsData`, encodes
  it to OTLP protobuf via `otel_pipeline_data`, and pushes it through
  `push_logs`, all from `wasm4otel_receive`. Release build: **~11 KB**.
- `severity_filter.zig` — a processor, the best-fitting role above:
  decodes the `LogsData` batch the host hands it, drops
  records below a severity threshold, re-encodes and forwards via
  `push_logs`. Exports `wasm4otel_process_logs` plus the
  `wasm4otel_alloc` / `wasm4otel_free` pair the host needs to hand a
  batch in. Decoding needs an allocator, so it runs everything
  through an arena over `std.heap.wasm_allocator` and tears it down on
  return. Release build: **~34 KB** — the decoder is what the other
  two don't pay for.

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
- **Exports** via `@export(&fn, .{ .name = "wasm4otel_…" })` — the
  wire names listed in `OtelPlugin.symbols` are the ones made
  visible.
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
| `std.crypto.random` via the OS path  | needs `random_get` — no entropy source at all here        |
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
their default of `0`. None of the guest exports take a timestamp
parameter, so a freestanding plugin that needs a real one has to
either get it from a new host import (a `host_now()` alongside
`host_log`) or move to the WASIp1 target.

Randomness has the same shape. `random_get` is a WASI call, so
there is no entropy source here at all — not even a seeded one —
which rules out generating trace IDs, span IDs or UUIDs in-plugin.
The host wires `crypto/rand.Reader` for WASI plugins
(see [`../../go/WAZERO.md`](../../go/WAZERO.md)), so a plugin that
needs IDs belongs in [`../wasip1/`](../wasip1/) or must take them
from the host.

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
    .{ .filename = "helloworld.zig", .symbols = &.{
        "wasm4otel_setup", "wasm4otel_start", "wasm4otel_receive", "wasm4otel_shutdown",
        // …plus the six batch exports and the allocator pair; see build.zig
    } },
    .{ .filename = "one_log.zig", .symbols = &.{"wasm4otel_receive"} },
    .{ .filename = "severity_filter.zig", .symbols = &.{ "wasm4otel_process_logs", "wasm4otel_alloc", "wasm4otel_free" } },
    // .{ .filename = "my_plugin.zig", .symbols = &.{ "wasm4otel_receive" } },
};
```

The symbols to list are the wire names for the role the plugin plays;
[`../README.md`](../README.md#adding-a-plugin) has the mapping.
`_start` does **not** belong in the list — wasm-lld exports the entry
symbol on its own.

The `host`, `guest`, and `otel_pipeline_data` modules are wired into
the freestanding loop by default — see `build.zig`.
