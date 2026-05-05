# WebAssembly — versions, proposals, and runtime support

Quick reference for which WASM features are part of which spec version
and which are tracked as separate "proposals". Wasmtime is used here
as the reference implementation because it is the most up-to-date
runtime; per-host coverage for this project lives in
[`go/WAZERO.md`](go/WAZERO.md), and toolchain coverage in
[`zig/WASM.md`](zig/WASM.md).

## How WASM evolves

The W3C ratifies WebAssembly in numbered **core spec versions**:

| Version  | Year | Status                                         |
| -------- | ---- | ---------------------------------------------- |
| **1.0**  | 2019 | W3C Recommendation. The "MVP".                 |
| **2.0**  | 2025 | W3C Recommendation. Adds 7 finished proposals. |
| **3.0**  | —    | Working draft, currently being assembled.      |

Between versions, new behaviour lives in **proposals**, each tracked
in its own repo under [`github.com/WebAssembly/`](https://github.com/WebAssembly).
Each proposal advances through phases:

| Phase | Name                  |
| ----- | --------------------- |
| 0     | Pre-Proposal          |
| 1     | Feature Proposal      |
| 2     | Proposed Spec Text    |
| 3     | Implementation Phase  |
| 4     | Standardize the Feature |
| 5     | The Feature is Standardized |

A proposal at phase 4–5 is what most runtimes actually implement; the
next core spec version bundles the phase-5 ones into the formal
recommendation.

## Wasmtime stability tiers

Wasmtime sorts implemented proposals into tiers (see
[`docs/stability-wasm-proposals.md`](https://github.com/bytecodealliance/wasmtime/blob/main/docs/stability-wasm-proposals.md)):

- **Tier 1** — enabled by default, fully tested and fuzzed.
- **Tier 2** — implemented but off by default; opt-in with `-W <name>`.
- **Tier 3** — early or partial implementation.
- **Untracked** — known proposals with no Wasmtime implementation yet.

## Master proposal table

Bundled into the core spec:

| Proposal                  | In version | Wasmtime |
| ------------------------- | ---------- | -------- |
| `mutable-globals`         | 1.0        | T1       |
| `sign-extension-ops`      | 2.0        | T1       |
| `nontrapping-fptoint`     | 2.0        | T1       |
| `multi-value`             | 2.0        | T1       |
| `bulk-memory-operations`  | 2.0        | T1       |
| `reference-types`         | 2.0        | T1       |
| `simd` (a.k.a. simd128)   | 2.0        | T1       |

Post-2.0, on track for 3.0 (phase 4 or 5):

| Proposal              | Phase | Wasmtime | What it adds                                                                    |
| --------------------- | ----- | -------- | ------------------------------------------------------------------------------- |
| `relaxed-simd`        | 5     | T1       | SIMD ops with implementation-defined results (faster on real hardware).         |
| `multi-memory`        | 5     | T1       | A module can declare more than one linear memory.                               |
| `tail-call`           | 5     | T1       | `return_call` / `return_call_indirect` instructions.                            |
| `extended-const`      | 5     | T1       | Arithmetic and global refs in constant initializer expressions.                 |
| `memory64`            | 4     | T1       | 64-bit memory indexing (separate `wasm64` ISA in some toolchains).              |
| `exception-handling`  | 4     | T2       | First-class try/throw/catch with tag types.                                     |
| `gc`                  | 4     | T2       | Heap-allocated structs/arrays managed by the runtime, plus type subtyping.      |
| `function-references` | 4     | T2       | Typed function references; prerequisite for `gc`.                               |
| `threads`             | 4     | T2       | Shared memory + atomic instructions.                                            |
| `component-model`     | 1–3   | T1       | High-level interface types and module composition (basis for WASIp2/p3).        |

Earlier-phase / partial in Wasmtime:

| Proposal              | Phase | Wasmtime | Notes                                                       |
| --------------------- | ----- | -------- | ----------------------------------------------------------- |
| `custom-page-sizes`   | 3     | T2       | Lets modules opt into smaller page sizes than 64 KiB.       |
| `wide-arithmetic`     | 3     | T2       | 128-bit add/sub/mul helpers.                                |
| `stack-switching`     | 3     | T3       | Coroutines / fibers. Wasmtime: x86_64 Linux only.           |

Tracked but not implemented in Wasmtime:

| Proposal                    | Phase  | Notes                                                              |
| --------------------------- | ------ | ------------------------------------------------------------------ |
| `branch-hinting`            | 3      | `(branch_hint likely/unlikely)` annotations.                       |
| `flexible-vectors`          | 1–2    | Variable-length SIMD.                                              |
| `memory-control`            | 1–2    | `memory.discard`-style hints to the host.                          |
| `shared-everything-threads` | 1–2    | Stronger threading model; supersedes `threads` in the long term.   |

## Component Model

The component model is technically a separate set of proposals layered
on top of core wasm. It defines:

- A binary format wrapping core wasm modules.
- An interface-types system (`record`, `variant`, `list<T>`, etc.).
- A linking model where components compose by importing/exporting
  typed instances.

WASI Preview 2 and Preview 3 are defined as component-model worlds —
see [`WASI.md`](WASI.md). Wasmtime exposes the component model
under Tier 1 and is the canonical implementation.

## Picking a target feature set

A useful framing: rather than picking a single "WASM version", you
pick a **feature set** that both your toolchain emits and your host
can consume. Most modern toolchains let you specify this as a list of
`-mattr=+feature` flags (LLVM, Clang, Zig) or via named CPU models.
Hosts likewise gate features behind config flags.

The portable defaults today are roughly **WASM 2.0**: assume any
runtime supports the Tier-1-by-default set above. Anything beyond
that needs verification on the target host.
