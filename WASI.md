# WASI — versions and worlds

The **WebAssembly System Interface** is the standard set of host
imports a wasm guest uses to talk to the outside world (files, time,
random, networking, …). It has gone through several incompatible
revisions; picking the right one matters for both toolchain output
and host runtime support.

This file lists the revisions and what's in them. Wasmtime is used as
the reference because it tracks the spec most closely; per-runtime
coverage is in [`go/WAZERO.md`](go/WAZERO.md).

## Revision lineage

| Revision    | Module name(s)                     | Format                       | Status                  |
| ----------- | ---------------------------------- | ---------------------------- | ----------------------- |
| **Preview 0** | `wasi_unstable`                  | Core wasm imports            | Historical — do not use |
| **Preview 1** | `wasi_snapshot_preview1`         | Core wasm imports            | Stable, ubiquitous      |
| **Preview 2** | `wasi:0.2.x` (component model)   | Component model worlds       | Stable since 2024       |
| **Preview 3** | `wasi:0.3.x`                     | Component model + async      | Draft / in development  |

The big break is between **Preview 1** and **Preview 2**: Preview 1 is
a flat list of host functions imported into a core wasm module;
Preview 2+ is defined in WIT (the component model's IDL) as a set of
*worlds*, each bundling a curated set of *interfaces*.

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
`zig/wasip1/log_generator.zig`).

## Preview 2 (`wasi:0.2.x`, "WASI 0.2")

Reorganized around the component model. Instead of one giant module,
WASI is split into many fine-grained interfaces and bundled into
**worlds** that describe a complete environment. Each world's contents
are defined in WIT.

Selected interfaces (all live under `wasi:` prefix):

| Interface           | Provides                                              |
| ------------------- | ----------------------------------------------------- |
| `wasi:clocks`       | Wall + monotonic clocks, timezone-free.               |
| `wasi:filesystem`   | Capability-style file/directory handles.              |
| `wasi:random`       | Secure + insecure random bytes.                       |
| `wasi:io`           | Streams (`input-stream`, `output-stream`) + pollables. |
| `wasi:sockets`      | TCP/UDP/IP, name resolution.                          |
| `wasi:cli`          | Stdio, env, args. Wraps the above for CLI programs.   |
| `wasi:http`         | Outgoing requests + incoming proxy handler.           |

Selected worlds:

| World                    | Use case                                                          |
| ------------------------ | ----------------------------------------------------------------- |
| `wasi:cli/command`       | Shell-style program. The Preview 2 successor to Preview 1.        |
| `wasi:http/proxy`        | HTTP request/response handler (think: serverless functions).      |
| `wasi:cli/run`           | Same as `command` minus stdin handling.                           |

Wasmtime support:
- `wasi:cli/command` — Tier 1, exposed via the default `wasmtime run`.
- `wasi:http/proxy` — Tier 1 since v18.0, exposed via `wasmtime serve`.
- The lower-level interfaces (`wasi:io`, `wasi:filesystem`, etc.) are
  reachable when embedding Wasmtime as a library.

## Preview 3 (`wasi:0.3.x`)

Still a draft. The headline change is **first-class async**: streams
and futures become part of the type system instead of being
emulated through `wasi:io` pollables. Worlds are being re-cut along
those lines.

Wasmtime: partial / experimental. Recent releases shipped an initial
implementation of the `wasi:tls@0.3.0-draft` interface; expect more to
land incrementally.

Picking Preview 3 today is appropriate for experiments and
contributions; production code should target Preview 2.

## Choosing a target

| Target                      | When                                                                                                                                            |
| --------------------------- | ----------------------------------------------------------------------------------------------------------------------------------------------- |
| `wasm32-freestanding`       | No syscalls needed at all (e.g. pure compute, embedded inside a host that grants its own imports — like `wasm4otel`'s freestanding plugin).     |
| `wasm32-wasip1`             | Want filesystem/clocks/random and broad runtime support including older toolchains. Most existing WASI ecosystem.                               |
| `wasm32-wasip2` (component) | Want the component model: typed interfaces, multiple modules composed, HTTP-proxy style deployments.                                            |
| `wasm32-wasip3`             | Tracking the spec; not yet ready for production.                                                                                                |

For embedding scenarios where the **host** defines its own imports —
which is exactly what `wasm4otel` does — choosing `freestanding` or
`wasip1` is mostly a matter of which language stdlib you want.
Freestanding gives you no stdlib runtime support; `wasip1` reactor mode
gives you a `_initialize` hook and lets the stdlib lazy-init.
