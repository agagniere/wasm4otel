# WebAssembly — versions, proposals, and runtime support

Quick reference for which WASM features are part of which spec version
and which are tracked as separate "proposals". Wasmtime is used here
as the reference implementation because it is the most up-to-date
runtime; per-host coverage for this project lives in
[`go/wazero/WAZERO.md`](go/wazero/WAZERO.md), and toolchain coverage in
[`zig/WASM.md`](zig/WASM.md).

## How WASM evolves

WebAssembly is cut into numbered **core spec versions** by the
Community Group, which the W3C then tracks — slowly — on the
Recommendation track:

| Version | Released | W3C Recommendation-track status                       |
| ------- | -------- | ----------------------------------------------------- |
| **1.0** | 2019     | W3C Recommendation, 5 Dec 2019.                       |
| **2.0** | 2022     | Candidate Recommendation Snapshot, 17 Dec 2024. Last 2.0-content draft: 16 Jun 2025. |
| **3.0** | Sep 2025 | Candidate Recommendation Draft (latest 11 Sep 2026).  |

Two things worth knowing about that table:

- **Neither 2.0 nor 3.0 is a W3C Recommendation.** Only 1.0 ever
  got one. 2.0 reached Candidate Recommendation *Snapshot* in
  December 2024 — frequently misreported as "2.0 became a W3C
  standard" — and stalled there. Don't wait on a REC as a maturity
  signal: engines shipped these features years before the paperwork.
- **There is no `wasm-core-3` shortname.** `www.w3.org/TR/wasm-core-2/`
  is still the Recommendation-track URL, but the drafts published
  under it now carry 3.0 content. The version in the document header
  is authoritative, not the URL.

Between versions, new behaviour lives in **proposals**, each tracked
in its own repo under [`github.com/WebAssembly/`](https://github.com/WebAssembly).
Each proposal advances through phases:

| Phase | Name                        |
| ----- | --------------------------- |
| 0     | Pre-Proposal                |
| 1     | Feature Proposal            |
| 2     | Proposed Spec Text          |
| 3     | Implementation Phase        |
| 4     | Standardize the Feature     |
| 5     | The Feature is Standardized |

Phase numbers are a poor proxy for "is it in the spec". Phase 4 is the
merge point: proposals that get there move to
[`finished-proposals.md`](https://github.com/WebAssembly/proposals/blob/main/finished-proposals.md)
and stop being listed as active. But the list and the spec draft aren't
in lockstep — `threads`, `wide-arithmetic` and `compact-import-section`
all sit at phase 4 on the active list without being in 3.0, and phase 5
today holds only JS-side proposals (JS Promise Integration, Web Content
Security Policy) that never touch the core spec. Read
`finished-proposals.md`, not the phase number, to answer "is this in
the spec".

## Wasmtime stability tiers

Wasmtime sorts implemented proposals into tiers (see
[`docs/stability-wasm-proposals.md`](https://github.com/bytecodealliance/wasmtime/blob/main/docs/stability-wasm-proposals.md)):

- **Tier 1** — enabled by default, fully tested and fuzzed.
- **Tier 2** — implemented but off by default; opt-in with `-W <name>`.
- **Tier 3** — early or partial implementation.
- **Untracked** — known proposals with no Wasmtime implementation yet.

## Master proposal table

Bundled into core spec 1.0 and 2.0:

| Proposal                 | In version | Wasmtime |
| ------------------------ | ---------- | -------- |
| `mutable-globals`        | 1.0        | T1       |
| `sign-extension-ops`     | 2.0        | T1       |
| `nontrapping-fptoint`    | 2.0        | T1       |
| `multi-value`            | 2.0        | T1       |
| `bulk-memory-operations` | 2.0        | T1       |
| `reference-types`        | 2.0        | T1       |
| `simd` (a.k.a. simd128)  | 2.0        | T1       |

Bundled into core spec 3.0 — all finished (merged at phase 4) and all
Tier 1 in Wasmtime except where noted:

| Proposal              | Wasmtime | What it adds                                                               |
| --------------------- | -------- | -------------------------------------------------------------------------- |
| `relaxed-simd`        | T1       | SIMD ops with implementation-defined results (faster on real hardware).    |
| `multi-memory`        | T1       | A module can declare more than one linear memory.                          |
| `tail-call`           | T1       | `return_call` / `return_call_indirect` instructions.                       |
| `extended-const`      | T1       | Arithmetic and global refs in constant initializer expressions.            |
| `function-references` | T1       | Typed function references; prerequisite for `gc`.                          |
| `gc`                  | T1       | Heap-allocated structs/arrays managed by the runtime, plus type subtyping. |
| `memory64`            | T1       | 64-bit memory indexing (separate `wasm64` ISA in some toolchains).         |
| `exception-handling`  | T1       | First-class try/throw/catch with tag types.                                |
| `branch-hinting`      | T3       | `(branch_hint likely/unlikely)` annotations. Off by default pending fuzzing. |

The 3.0 release is what "modern wasm" now means in practice: a 64-bit
address space, multiple memories, exception handling, and GC'd struct
and array types all became baseline here.

Phase 4 but **not** merged into a spec release yet:

| Proposal                 | Wasmtime | Notes                                                                       |
| ------------------------ | -------- | --------------------------------------------------------------------------- |
| `wide-arithmetic`        | T1       | 128-bit add/sub/mul helpers.                                                |
| `threads`                | T2       | Shared memory + atomics. Unfinished and unfuzzed in Wasmtime — no pooling-allocator support, and fuzzing waits on `shared-everything-threads`. |
| `compact-import-section` | ❌       | Smaller encoding for the import section.                                    |

Phase 3 (implementation phase):

| Proposal             | Wasmtime | Notes                                                        |
| -------------------- | -------- | ------------------------------------------------------------ |
| `custom-page-sizes`  | T2       | Lets modules opt into smaller page sizes than 64 KiB.        |
| `stack-switching`    | T3       | Coroutines / fibers. Wasmtime: x86_64 Linux only.            |
| `esm-integration`    | —        | Loading wasm modules as ES modules. Web embedding only.      |
| `custom-descriptors` | —        | JS interop for GC'd types (prototypes, `instanceof`). Web embedding only. |

Earlier phases, none implemented in Wasmtime:

| Proposal                    | Phase | Notes                                                            |
| --------------------------- | ----- | ---------------------------------------------------------------- |
| `fp16`                      | 2     | Half-precision float ops. Zig can emit these — see [`zig/WASM.md`](zig/WASM.md). |
| `compilation-hints`         | 2     | Annotations to steer engine tiering decisions.                   |
| `acquire-release-atomics`   | 2     | Weaker (cheaper) memory orderings than the `threads` seq-cst set. |
| `flexible-vectors`          | 1     | Variable-length SIMD.                                            |
| `memory-control`            | 1     | `memory.discard`-style hints to the host.                        |
| `shared-everything-threads` | 1     | Stronger threading model; supersedes `threads` in the long term. |

Two distinct absences above: ❌ means Wasmtime explicitly tracks the
proposal as unimplemented (it does so for exactly four —
`flexible-vectors`, `memory-control`, `shared-everything-threads`, and
`compact-import-section`), while — means the proposal doesn't appear in
its tier tables at all.

## Component Model

The component model is technically a separate set of proposals layered
on top of core wasm. It defines:

- A binary format wrapping core wasm modules.
- An interface-types system (`record`, `variant`, `list<T>`, etc.).
- A linking model where components compose by importing/exporting
  typed instances.
- Since the WASI 0.3 cycle, async primitives in the Canonical ABI:
  `async func`, `stream<T>`, and `future<T>`.

It sits at **phase 1** in the CG's proposal list, which understates
reality considerably: Wasmtime exposes it under Tier 1, enabled by
default, and it is the canonical implementation. Individual
sub-features are gated separately — `async` is on by default, while
`map`, `implements`, stackful async, `threading`, `fixed-length-lists`,
`memory64`, `error-context`, `values`, and the various naming
extensions are opt-in.

WASI Preview 2 and Preview 3 are defined as component-model worlds —
see [`WASI.md`](WASI.md).

## Picking a target feature set

A useful framing: rather than picking a single "WASM version", you
pick a **feature set** that both your toolchain emits and your host
can consume. Most modern toolchains let you specify this as a list of
`-mattr=+feature` flags (LLVM, Clang, Zig) or via named CPU models.
Hosts likewise gate features behind config flags.

For this project the binding constraint is not the spec, and not Zig —
it's wazero, which implements **2.0 and nothing from 3.0 by default**,
with five proposals available as experimental opt-ins. So the portable
target here stays roughly WASM 2.0 even though 3.0 features are
shipping in Wasmtime and browsers. See [`go/wazero/WAZERO.md`](go/wazero/WAZERO.md)
for the exact list and [`zig/WASM.md`](zig/WASM.md) for which Zig CPU
models stay inside it.
