# The WIT interface

`wasm4otel.wit` states the plugin ABI declaratively. It is a *proposal*:
nothing in the repo consumes it yet, and no plugin is built against it.
Both hosts still speak the imperative core-wasm ABI documented in
[`AGENTS.md`](../AGENTS.md); this file is what that contract looks like
once the Component Model carries it instead.

It exists because the Component Model is where this ABI wants to end up,
and `go/wazy` is the first host here that can actually run components.
Writing the contract down in WIT is the step that turns "we should
migrate" into something reviewable.

Validate it with:

```sh
wasm-tools component wit interface/wasm4otel.wit
```

## Why a .wit at all

The current arrangement keeps the contract in three places that have to
be hand-synchronised: `Component.LoadPlugin`'s string literals, the Zig
guests' `export fn` declarations, and the prose table in `AGENTS.md`. A
mismatch is not a build error — it is a `nil` function pointer, or a
plugin that loads and then misbehaves.

A `.wit` makes the contract one machine-readable artifact that both
sides generate from, so a mismatch fails at build or link time.

## What maps 1:1

| Core ABI                                 | WIT                                          |
|------------------------------------------|----------------------------------------------|
| `env` module                             | `interface host`                             |
| `host_log(i32, ptr, size)`               | `host.log(level: log-level, message: string)` |
| `push_logs(ptr, size) -> i32`            | `host.push-logs(payload) -> result<_, push-error>` |
| `interruptible_sleep_ms(u32) -> u32`     | `host.interruptible-sleep(millis) -> sleep-outcome` |
| `get_config(ptr, size) -> u32`           | `host.get-config() -> option<string>`        |
| `wasm4otel_setup() -> i32`               | `lifecycle.setup() -> result<_, setup-error>` |
| `wasm4otel_start() -> i32`               | `lifecycle.start() -> result<_, run-error>`  |
| `wasm4otel_shutdown()`                   | `lifecycle.shutdown()`                       |
| `wasm4otel_receive() -> i32`             | `receive-loop.run() -> result<_, run-error>` |
| `wasm4otel_process_logs(ptr, size)`      | `process-logs.handle(payload)`               |
| `wasm4otel_export_logs(ptr, size)`       | `export-logs.handle(payload)`                |

## What the Component Model deletes outright

These are not design choices in the WIT — they are parts of the current
ABI that stop having anything to describe.

- **`wasm4otel_alloc` / `wasm4otel_free`.** The whole reason they exist
  is that the host has to reach into guest linear memory to hand over a
  batch. Under the Canonical ABI the payload arrives as an owned
  `list<u8>` and the runtime owns the copy. The
  alloc → write → call → free sequence in `callBatch` becomes one call.
- **`push-error`'s "bad memory read" code.** Code 1 means the host
  could not read `(ptr, size)` out of guest memory. With no raw pointers
  crossing the boundary, that failure mode is gone, which is why
  `push-error` has three cases and not four.
- **The `get_config` probe-then-read dance.** Today the guest calls
  `get_config(0, 0)` to learn the size, allocates, then calls again —
  and the host has to refuse to write a truncated JSON document.
  `option<string>` is one call that cannot be half-done.
- **`WithStartFunctions("_start", "_initialize")`.** The host currently
  passes both names because Zig 0.16 emits neither automatically and
  the plugin picks one. A component declares its own instantiation.
- **Export-table sniffing for role disambiguation.** The ABI's central trick
  is that `wasm4otel_process_logs` vs `wasm4otel_export_logs` tells the
  host which role a plugin was built for. A world *is* that
  declaration, and a mismatch is caught at link time instead of
  producing the boot-time error `validateModeExports` and
  `ValidateLogsExport` are written to generate.

## Open questions to settle

Listed roughly in order of how much they change.

### 1. Do we model telemetry, or keep passing OTLP bytes?

`otlp-payload = list<u8>` is the conservative choice, and it keeps
protobuf as the thing both sides already speak. The alternative is
modelling `resource-logs` / `log-record` / `any-value` as WIT records,
which would let a plugin mutate a field without linking a protobuf
library at all — the single biggest quality-of-life change available to
guest authors, and by far the largest amount of work. It also means
tracking the OTLP schema in WIT by hand.

Worth noting the middle path: keep bytes now, and treat the modelled
version as a second interface that plugins can opt into later.

### 2. Should errors carry a message?

The WIT keeps `setup-error` and `run-error` as bare enums, faithful to
the i32 codes, which is why the host's messages all end in "see
plugin logs". WIT can do better:

```wit
variant setup-error {
    failed(string),
    invalid-config(string),
}
```

Then a bad `plugin_config` fails the collector's boot with the plugin's
own explanation of what was wrong with it, instead of an error that
tells the operator to go read a different stream. This is cheap and the
payoff is mostly on the operator's side, so it is the most likely thing
to change before the WIT becomes real.

### 3. Seven worlds, or three?

The WIT has one world per (role, signal) pair because a WIT interface is
all-or-nothing: exporting `process-batch` would oblige a plugin to
handle logs *and* metrics *and* traces, whereas the core ABI lets a plugin export
`wasm4otel_process_logs` alone. Per-signal interfaces preserve that, and
they line up with how the host validates: `Load` checks only what the
*mode* needs, and the factory's `createLogs` then calls
`ValidateLogsExport` for the one signal that pipeline section wired.

The cost is boilerplate: three near-identical `process-*` interfaces,
three `export-*`, eight worlds. Collapsing to `processor` / `exporter` /
`receiver` would be far more readable, at the price of forcing
single-signal plugins to implement two no-op functions.

### 4. Is the `lifecycle` / role split right?

Every world exports `lifecycle` plus exactly one role interface. That is
a clean separation, but it does mean `setup` and `start` are declared
away from the role they serve. The alternative — folding the lifecycle
functions into each role interface — removes the indirection and
duplicates three declarations per role.

### 5. Does `host` stay one interface?

It is currently one flat interface holding logging, three pushes, sleep
and config. Splitting it (`logging`, `pipeline`, `config`) would let a
world import only what it needs — a processor has no use for
`interruptible-sleep`, and an exporter's `push-*` calls are guaranteed
to fail with `no-consumer`. Against that: the small uniform import set
is deliberate, and a plugin that needs a bespoke host function for its
core capability is a sign it is a bad fit for wasm4otel rather than a
sign the ABI needs widening.

### 6. Where does `interruptible-sleep` go?

It exists because a WASI sleep is not cancellable, so the host refuses
to wire `WithSysNanosleep` and gives the guest a cancellable sleep
instead. WASI 0.2 has `wasi:io/poll`, whose `pollable` is cancellable,
so a receive loop could pace itself against a real clock and drop this
import entirely. That needs checking against what wazy's wasip2
implementation actually offers.

## Getting from here to there

Not part of this PR — sketching it out is how we find out whether the
WIT above is the right shape.

1. Generate the Go host bindings from the WIT and check them against
   `go/wazy`'s hand-written `env` module, which is the cheapest way to
   find out where the mapping is wrong.
2. Port one guest to a component and run it under
   `wazy/component.Instantiate`. `zig/freestanding/severity_filter.zig`
   is the smallest candidate — but Zig has no component-model backend
   today, so this likely means `wasm-tools component new` over a wasip1
   build, or a Rust guest to prove the path before porting Zig.
3. Only once a component actually runs: add a component code path to
   `go/wazy` alongside the core-module one, and let the two coexist
   while the guests migrate.
