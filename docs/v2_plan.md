# ABI v2 plan

Captures today's design discussion. The current ABI ("v1") works but
has rough edges around naming, role disambiguation, and lifecycle
hooks for guest-side config validation. This file is the concrete
plan to fix all of that in one coordinated change, plus the follow-up
work it surfaces.

## Goals

1. **Uniform naming.** Every host-called guest export gets a
   `wasm4otel_` prefix so the wasm export table is self-evidently the
   ABI surface.
2. **Load-time role disambiguation.** A plugin's export shape alone
   should tell the host which role it's designed for (receiver vs
   processor vs exporter), so the wrong YAML wiring fails fast with
   a clean message instead of silently running the wrong path.
3. **First-class config validation hook.** Today the plugin only
   gets to validate config inside `start`, which runs at
   `Component.Start` time — *after* the factory has returned success
   to the framework. We want a guest-side validation hook that runs
   inside the factory's `createX`, so a bad YAML config rejects the
   collector at boot, not at first batch.
4. **Stay aligned with OTel naming.** The lifecycle exports keep the
   `Start` / `Shutdown` shape OTel itself uses for components.

## ABI changes

### Lifecycle exports (all prefixed)

| Export               | Signature   | When called                                   | Role          |
|----------------------|-------------|-----------------------------------------------|---------------|
| `wasm4otel_setup`    | `() -> i32` | Factory `createX`, after instantiation        | All           |
| `wasm4otel_start`    | `() -> i32` | `Component.Start`, sync, must return promptly | All           |
| `wasm4otel_receive`  | `() -> i32` | `Component.Start`, on a goroutine             | Receiver only |
| `wasm4otel_shutdown` | `() -> ()`  | `Component.Shutdown`, sync                    | All           |

`wasm4otel_setup` is the new slot. Reads the YAML `plugin_config`
via `get_config`, validates it, returns success / failure /
invalid_config. The factory's `createX` surfaces non-zero as an
error returned to the OTel framework — the collector refuses to
finish booting with a clear message.

`wasm4otel_start` is the OTel `Component.Start` hook. **Always
short-lived** in every mode — must return promptly so the pipeline
can begin work. Used for late-stage init that didn't fit in setup
(e.g., opening files now that config is known to be valid). No
dual meaning across modes.

`wasm4otel_receive` is the receiver's long-running loop. The host
spawns it on a goroutine after `wasm4otel_start` returns success.
Receivers are signal-agnostic at the entry point — a single
receive loop calls `push_logs` / `push_metrics` / `push_traces` as
needed (matches how OTLP and other multi-signal receivers in core
work). Role-tagged name makes the receiver-only nature obvious at
the source-code level, in line with `process_*` / `export_*` tagging
for the other roles.

`wasm4otel_shutdown` replaces today's `stop`. Prefixing makes the
libc collision on `shutdown` (POSIX socket call) a non-issue.

### Batch exports (role-typed, signal-typed)

| Export                                            | Required for             |
|---------------------------------------------------|--------------------------|
| `wasm4otel_process_logs` / `_metrics` / `_traces` | Processor on that signal |
| `wasm4otel_export_logs` / `_metrics` / `_traces`  | Exporter on that signal  |

Today's single `consume_<signal>` splits into two role-typed
versions. The host knows from the export table alone which role the
plugin is designed for — no env var or runtime declaration needed.

A plugin that legitimately works as both processor and exporter
exports both, typically delegating to a shared internal function:

```zig
fn handleLogs(ptr: [*]const u8, size: usize) i32 { ... }
export fn wasm4otel_process_logs(ptr: [*]const u8, size: usize) i32 { return handleLogs(ptr, size); }
export fn wasm4otel_export_logs(ptr: [*]const u8, size: usize) i32  { return handleLogs(ptr, size); }
```

### Allocator exports (unchanged)

`wasm4otel_alloc(size: u32) -> u32` and
`wasm4otel_free(ptr: u32, size: u32)` keep their current names —
already prefixed because of the wasi-libc `free` collision.

### Host imports (no rename)

`host_log` / `push_<signal>` / `interruptible_sleep_ms` /
`get_config` stay as-is. They're scoped by the `"env"` module name
already; adding `wasm4otel_` would be the prefix-twice antipattern.

## Load-time validation rules

Validation is **additive**: each factory's `createX` checks that
the plugin exports what *that role* needs, never that the plugin
doesn't export something for another role. A single `.wasm` can
export `wasm4otel_receive` + `wasm4otel_process_logs` +
`wasm4otel_export_metrics` simultaneously and be deployable in three
YAML sections at once, each as its own `Component` instance playing
exactly one role. (The wazero single-occupancy rule binds per-
instance-runtime, not per-export-set, so multi-role plugins are
safe.)

Rules per factory:

1. **Receiver factory** requires `wasm4otel_receive`. Without it,
   the plugin has no way to push telemetry into the pipeline.
2. **Processor factory on signal `<S>`** requires
   `wasm4otel_process_<S>` plus `wasm4otel_alloc` /
   `wasm4otel_free`.
3. **Exporter factory on signal `<S>`** requires
   `wasm4otel_export_<S>` plus `wasm4otel_alloc` / `wasm4otel_free`.

After the per-role checks pass, the host calls `wasm4otel_setup` (if
exported). If setup returns non-zero, the factory's `createX`
returns the corresponding error.

Error messages report what's *missing for the role the operator
wired*, never what's *present but incompatible* — the goal is to
guide the operator to either fix the YAML or rebuild the plugin
with the right export.

## Return-code allocation

```zig
pub const SetupResult = enum(i32) {
    success = 0,
    failure = 1,
    invalid_config = 2,
    _,
};

pub const StartResult = enum(i32) {
    success = 0,
    failure = 1,
    _,
};
```

Setup carries the config-validation enum because that's where YAML
mistakes get rejected. Start simplifies to "did work start" — by
that point config has already been validated.

Both enums are non-exhaustive (the `_` tail) so additional codes
later don't break old plugins. The host's wording falls back to
generic "failure" for unknown codes.

## Implementation order (one PR)

1. Add `wasm4otel_setup` lookup + invocation inside `wasm4otel.Load`.
   Wire `SetupResult` constants and a `setupResultError` helper
   mirroring the existing `startResultError`. Factory `createX`
   already returns the resulting error.
2. Rename existing exports in `component.go`'s `LoadPlugin`
   lookups: `start` → `wasm4otel_start`, `stop` →
   `wasm4otel_shutdown`. Add a `wasm4otel_receive` lookup.
3. Refactor `Component.Start`: always call `wasm4otel_start`
   synchronously first; surface rc. Then if `wasm4otel_receive` is
   present, spawn it on a goroutine (existing receiver-mode logic
   moves under the receive lookup). `Component.Shutdown` waits for
   the receive goroutine (when applicable) then calls
   `wasm4otel_shutdown`.
4. Split `consume_<signal>` lookups into
   `wasm4otel_process_<signal>` and `wasm4otel_export_<signal>`.
   Component gets six function slots instead of three. `ConsumeLogs`
   / `ConsumeMetrics` / `ConsumeTraces` pick the right one based on
   `ComponentMode`.
5. Update per-role validators: receiver factory requires
   `wasm4otel_receive`; processor factory requires
   `wasm4otel_process_<signal>` + alloc/free; exporter factory
   requires `wasm4otel_export_<signal>` + alloc/free. No
   "exclusion" checks — additive only.
6. Update Zig templates:
   - `wasip1/log_generator.zig`: `start` → `wasm4otel_receive`
     (the loop), `stop` → `wasm4otel_shutdown`. Optionally add a
     `wasm4otel_start` if anything wants late-init.
   - `freestanding/severity_filter.zig`: `consume_logs` →
     `wasm4otel_process_logs`.
   - `zig/src/guest.zig`: update the lifecycle rc enum names if
     needed; the non-exhaustive `_` stays.
7. Docs sweep: AGENTS.md, top README, go/README.md. The exports
   table and rc table both grow.

Estimated diff: 300–450 lines, mostly mechanical.

## Out of scope (follow-ups)

These came up today but don't belong in the ABI rename PR.

### Shared receiver instance across signal pipelines

Confirmed today by reading `otlpreceiver/factory.go`: receivers
that hold a shared resource (a port, a DB connection, our wazero
runtime) use a package-level `sharedcomponent.NewMap[*Config, V]`
to dedup. Each `createX` calls `LoadOrStore`, then attaches a
per-signal consumer to the existing instance via a
`Register<Signal>Consumer` method.

Both `go.opentelemetry.io/collector/internal/sharedcomponent` and
`github.com/open-telemetry/opentelemetry-collector-contrib/internal/sharedcomponent`
exist (the two have diverged), but both are in `internal/` — neither
is importable from outside its parent module. We need our own
equivalent: ~50 lines, no `componentstatus.Reporter` plumbing
required.

Required before any serious receiver plugin: today, a wasm4otel
receiver wired into logs + metrics + traces pipelines would
instantiate the .wasm three times, run `_initialize` three times,
and spawn three independent `start` goroutines.

Component-side changes that come with it:
- `Register<Signal>Consumer` setters replacing direct field
  assignment to `NextConsumer*`.
- The current direct-assign is racy if hot-reload ever lands; for
  now it's startup-only and safe.

### Single shared wazero runtime

Today every `Component` builds its own `wazero.Runtime`. A shared
runtime across all components would save memory and startup time.
Adjacent to the sharedcomponent work but independent — sharedcomponent
dedups *components*; this would dedup *runtimes*.

### Wazero compiler mode

Done, as the `engine` YAML field: `auto` (default), `interpreter`,
`compiler`. The default was `interpreter` until informal measurement
put the interpreter at least an order of magnitude behind the
compiler, which is more than enough to pay for wazero's probe;
`auto` takes the compiler where the platform supports it and falls
back on its own everywhere else. No rigorous benchmark exists yet, so
a proper harness is still worth building.

### WASIp2 / arcjet/gravity

Transpile WASI components to wasip1 so wazero can run them. Unblocks
the Component Model migration before wazero ships native components
support.

### Go-side unit tests

Currently zero. A `consumertest`-sink-based integration test against
a tiny `.wasm` fixture would catch ABI regressions.

### Informative `WASM4OTEL_COMPONENT_TYPE` env var

Set `WASM4OTEL_COMPONENT_TYPE` (values `receiver` / `processor` /
`exporter`) in the wazero `ModuleConfig.WithEnv(...)` at
instantiation time. Purely informative — the role is already
determined by which export the host calls, so this isn't used for
load-time validation or runtime role-refusal. Available to WASI
plugins that want it; freestanding plugins can't read env vars.

A signal-type env var is intentionally not planned: incompatible
with the planned sharedcomponent dedup.

## Rejected during today's discussion (for the record)

- **Dual-meaning `start` (loop for receivers, sync init for others).**
  Originally kept for OTel alignment, but a separate
  `wasm4otel_receive` export turned out cleaner — `start` now means
  one thing in every mode (short-lived init), and the long-running
  loop is named for its role. Better than the dual-meaning shape we
  briefly settled on.
- **Generic `wasm4otel_run` for the long-running loop.** Considered
  alongside `wasm4otel_receive`. Lost because the role-tagged name
  prevents the "I'll export run from my exporter for background
  work" misconception, and matches the `process_*` / `export_*`
  tagging the other roles use.
- **`setup`/`cleanup` rename of start/stop.** Would break the OTel
  alignment. Adding `setup` as a *new* hook is fine.
- **`wasm4otel_receive_<signal>` for receivers.** Multiple loops per
  instance would either race on wazero single-occupancy or serialize
  via callMu and starve each other. Receivers are genuinely signal-
  agnostic at the entry point; signal is per-`push_<signal>`-call.
- **Env-var-driven role validation (`WASM4OTEL_COMPONENT_TYPE` +
  rc=3 `unsupported_role` in setup).** Initially proposed to let
  the plugin self-validate its role. Made obsolete by the
  `process_<signal>` / `export_<signal>` split — the export table
  tells the host the role directly, no runtime check needed. The
  component-type env var still lives on as a follow-up, but as
  informational only.
- **`_initialize` (WASI reactor entry) for config validation.**
  Fixed `() -> ()` signature; the only way to fail is to trap, which
  poisons the instance and surfaces as `wasm trap: unreachable`
  instead of a useful diagnostic. Setup gives us a return code and
  graceful semantics.
- **Reading the OTel collector's `internal/sharedcomponent` directly.**
  Blocked by Go's `internal/` rule. Roll our own.
