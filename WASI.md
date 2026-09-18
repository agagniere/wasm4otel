# WASI — versions and worlds

The **WebAssembly System Interface** is the standard set of host
imports a wasm guest uses to talk to the outside world (files, time,
random, networking, …). It has gone through several incompatible
revisions; picking the right one matters for both toolchain output
and host runtime support.

This file lists the revisions and what's in them. Wasmtime is used as
the reference because it tracks the spec most closely; per-runtime
coverage is in [`go/wazero/WAZERO.md`](go/wazero/WAZERO.md).

## Revision lineage

| Revision      | Module / package name          | Format                  | Status                                   |
| ------------- | ------------------------------ | ----------------------- | ---------------------------------------- |
| **Preview 0** | `wasi_unstable`                | Core wasm imports       | Historical — do not use                  |
| **Preview 1** | `wasi_snapshot_preview1`       | Core wasm imports       | Stable, ubiquitous                       |
| **Preview 2** | `wasi:0.2.x` (component model) | Component model worlds  | Stable. Line ended at 0.2.12, 2 Jun 2026 |
| **Preview 3** | `wasi:0.3.x`                   | Component model + async | **Stable since 0.3.0, 11 Jun 2026**      |

The big break is between **Preview 1** and **Preview 2**: Preview 1 is
a flat list of host functions imported into a core wasm module;
Preview 2+ is defined in WIT (the component model's IDL) as a set of
*worlds*, each bundling a curated set of *interfaces*.

The second break, 0.2 → 0.3, is narrower but touches every interface:
async moved out of WASI and into the component model itself.

Since 0.3.0, WASI ships on a **release train** — patch releases every
two months on the second Tuesday, carrying whatever is ready, with
larger or breaking work held for a milestone release. 0.3.1 landed
11 Aug 2026. There is no date for WASI 1.0; the roadmap says only
that it, like 0.3.0, would likely be scheduled off-train.

## Preview 1 (`wasi_snapshot_preview1`)

The classic POSIX-flavoured ABI: ~50 host functions covering files,
clocks, environment, random, polling. Designed for short-lived "command"
modules (think: `cat`, `wc`) and for "reactor" modules that expose
their own start/stop entrypoints.

Two execution models for guests:
- **Command** (`_start` entrypoint, single-shot)
- **Reactor** (`_initialize` entrypoint, host calls exports later)

Wasmtime: fully supported. This is what the Wasmtime CLI defaults
to for plain `.wasm` files.

This is the WASI flavour `wasm4otel` plugins target today (see
`zig/wasip1/log_generator.zig`), and — because wazero implements no
component model — the only one they *can* target.

## Preview 2 (`wasi:0.2.x`, "WASI 0.2")

Reorganized around the component model. Instead of one giant module,
WASI is split into many fine-grained interfaces and bundled into
**worlds** that describe a complete environment. Each world's contents
are defined in WIT.

Selected interfaces (all live under the `wasi:` prefix):

| Interface         | Provides                                               |
| ----------------- | ------------------------------------------------------ |
| `wasi:clocks`     | Wall + monotonic clocks, timezone-free.                |
| `wasi:filesystem` | Capability-style file/directory handles.               |
| `wasi:random`     | Secure + insecure random bytes.                        |
| `wasi:io`         | Streams (`input-stream`, `output-stream`) + pollables. |
| `wasi:sockets`    | TCP/UDP/IP, name resolution.                           |
| `wasi:cli`        | Stdio, env, args. Wraps the above for CLI programs.    |
| `wasi:http`       | Outgoing requests + incoming proxy handler.            |

`wasi:io` is the one to note: it exists only in 0.2. See below.

Worlds:

| World              | Use case                                                     |
| ------------------ | ------------------------------------------------------------ |
| `wasi:cli/command` | Shell-style program. The Preview 2 successor to Preview 1.   |
| `wasi:http/proxy`  | HTTP request/response handler (think: serverless functions). |

Each package also defines an aggregating `imports` world
(`wasi:cli/imports` and friends) that bundles its imports without
requiring any export.

`wasi:cli/run` is easy to mistake for a world — it's an **interface**,
the one holding `run: func() -> result` that the `command` world
requires as its export. Worlds can only be imported or `include`d,
never exported.

Wasmtime support:
- `wasi:cli/command` — Tier 1, exposed via the default `wasmtime run`.
- `wasi:http/proxy` — Tier 1 since v18.0, exposed via `wasmtime serve`.
- The lower-level interfaces (`wasi:io`, `wasi:filesystem`, etc.) are
  reachable when embedding Wasmtime as a library.

## Preview 3 (`wasi:0.3.x`, "WASI 0.3")

Released 11 Jun 2026 and **stable** — the subgroup voted to ratify it,
so components compiled against it keep working. The headline change is
**first-class async**: streams and futures are part of the component
model's Canonical ABI instead of being emulated through `wasi:io`
pollables.

Three new Canonical ABI primitives:

| Primitive   | Role                                                                             |
| ----------- | -------------------------------------------------------------------------------- |
| `async func` | Declared async in WIT; the runtime owns scheduling and suspension. Bindings surface it as `async fn` (Rust), a `Promise` (JS), a coroutine (Python). |
| `stream<T>` | Typed async data channel. A *value*, passable across component boundaries.       |
| `future<T>` | Single-value async completion. Replaces `pollable`.                              |

The motivation was composition, not ergonomics. Under 0.2 a `pollable`
is scoped to one component instance, so in an `A → B → Host` chain B
cannot relay host wake-ups up to A — each component needs its own
event loop and those loops can't coordinate. WASI calls this the
**sandwich problem**: 0.2 could express async but could not compose it
across component boundaries. Moving async into the Canonical ABI puts
the host in charge of one event loop shared by everything, so
composition depth stops mattering.

**`wasi:io` is gone.** There is no 0.3 version of the package; its job
now belongs to the component model:

| WASI 0.2                     | WASI 0.3                 |
| ---------------------------- | ------------------------ |
| `resource pollable`          | `future<T>`              |
| `resource input-stream`      | `stream<u8>`             |
| `resource output-stream`     | `stream<u8>` (write direction) |
| `poll(list<pollable>)`       | `await` on a future      |
| `subscribe()` on a resource  | return a `future`        |
| `start-foo` / `finish-foo`   | `foo: async func(...)`   |

Two idioms recur. **Stream-plus-future** pairs a `stream<T>` with a
future that resolves to success or error once the stream closes — used
by stdin, filesystem reads, TCP receives, directory listings. And the
**write direction flips**: where 0.2 handed you an `output-stream` to
write into imperatively, 0.3 has you pass in a `stream<u8>` and get
back a `future<result>`.

Per-package changes worth knowing:

- **`wasi:http`** — most reworked. Nine request/response/body/trailers
  resources collapse into unified `request` and `response` with
  `stream<u8>` bodies and `future` trailers. The `proxy` world is
  replaced by **`service`**, and a new **`middleware`** world imports
  the handler interface so request-path middleware is first class.
- **`wasi:sockets`** — seven interfaces down to two: a unified `types`
  holding both `tcp-socket` and `udp-socket`, plus `ip-name-lookup`.
  The `network` resource is **removed** — network access is granted at
  the world level instead of threaded through every call. TCP `listen`
  returns `stream<tcp-socket>`.
- **`wasi:clocks`** — renamed toward POSIX/Rust convention:
  `wall-clock` → `system-clock`, `datetime` → `instant`. `instant`'s
  seconds field became `s64` so pre-epoch timestamps are expressible.
  `subscribe-*` gave way to `wait-until` / `wait-for`.
- **`wasi:cli`** — stdio is stream-plus-future; `run` is an
  `async func`.
- **`wasi:filesystem`** — nearly every `descriptor` method is async;
  `read-directory` returns `stream<directory-entry>`.
- **`wasi:random`** — `len` renamed `max-len`, making explicit that
  implementations may return fewer bytes and callers must loop.

Worlds: `wasi:cli/command`, `wasi:http/service`, `wasi:http/middleware`.

Wasmtime support:
- **46+** — final 0.3.0, with WASI 0.3 and `component-model-async`
  enabled by default.
- **43–45** — the `0.3.0-rc-2026-03-15` snapshot.
- **41–42** — the `0.3.0-rc-2026-01-06` snapshot.
- 41 through 45 all need `-Sp3 -W component-model-async=y`.
- `wasmtime serve` takes either revision, falling back to 0.2's
  `wasi:http/proxy` when a component doesn't export 0.3's `service`.

Migration isn't forced: a host may implement both revisions, or
virtualize 0.2 on top of 0.3 primitives. Most of the mechanical work
is swapping `wasi:io` types for their component-model counterparts and
collapsing `start-foo`/`finish-foo` pairs into one `async func`. Pin
the toolchain and bindings generator to the same WIT version —
version-aware linking isn't universal yet, and a mismatch shows up as
a puzzling `wrong type` error at instantiation.

## Choosing a target

| Target                      | When                                                                                                                                        |
| --------------------------- | ------------------------------------------------------------------------------------------------------------------------------------------- |
| `wasm32-freestanding`       | No syscalls needed at all (e.g. pure compute, embedded inside a host that grants its own imports — like `wasm4otel`'s freestanding plugin). |
| `wasm32-wasip1`             | Want filesystem/clocks/random and broad runtime support including older toolchains. Most existing WASI ecosystem.                          |
| `wasm32-wasip2` (component) | Want the component model: typed interfaces, multiple modules composed, HTTP-service style deployments.                                      |
| `wasm32-wasip3` (component) | Want native async — composable streams and futures. Spec is stable; guest toolchain support is still landing.                               |

The spec being stable no longer decides p2 vs p3 — toolchains do, and
they vary sharply. Rust carries `wasm32-wasip1`, `wasm32-wasip2` and
`wasm32-wasip3` as Tier 2 targets with `std`; Zig's stdlib implements
only Preview 1, with p2/p3 reachable solely by hand-written
component-model glue (see [`zig/WASM.md`](zig/WASM.md)). Check the
specific toolchain before assuming a revision is available.

For embedding scenarios where the **host** defines its own imports —
which is exactly what `wasm4otel` does — choosing `freestanding` or
`wasip1` is mostly a matter of which language stdlib you want.
Freestanding gives you no stdlib runtime support; `wasip1` reactor mode
gives you a `_initialize` hook and lets the stdlib lazy-init.

Preview 2 and Preview 3 are both off the table for this repo today
regardless of toolchain: wazero implements no component model, so it
cannot load a component at all. The routes out are swapping the
runtime or transpiling components down to wasip1 — see
[`go/wazero/WAZERO.md`](go/wazero/WAZERO.md) and `docs/v2_plan.md`.
