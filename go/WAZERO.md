# wazero — what we get from the host runtime

[wazero](https://github.com/tetratelabs/wazero) is the wasm runtime
embedded by `wasm4otel`'s Go side. It's the only major pure-Go wasm
runtime, which is why it fits a collector receiver: no cgo, no
platform-specific build steps, no shared library to ship.

This file covers which wasm features wazero implements, and how that
constrains the `.wasm` modules a collector can load. For the master
list of WASM proposals see [`../WASM.md`](../WASM.md); for WASI
revisions see [`../WASI.md`](../WASI.md).

Pinned version: **v1.12.0** (see `go.mod`), the latest release as of
this writing (29 May 2026).

## Runtime modes

wazero has two execution backends:

- **Interpreter** — pure Go, runs everywhere Go runs.
- **Compiler** — JIT-style ahead-of-time compilation to native code.
  Available on `linux/darwin/windows` × `amd64/arm64`.

`wasm4otel` currently uses the interpreter (see
[`go/README.md`](README.md) for the rationale and how to switch).
Both backends implement the same feature set — including v1.12.0's new
proposals, which have real lowerings in the compiler frontend, not
stubs — so the choice is purely performance vs portability. As of
v1.12.0 wazero also falls back to the interpreter automatically when
executable `mmap` is unavailable.

## Core feature support

Source of truth: [`api/features.go`](https://github.com/tetratelabs/wazero/blob/main/api/features.go)
and [`experimental/features.go`](https://github.com/tetratelabs/wazero/blob/main/experimental/features.go).

### Bundled in `CoreFeaturesV1` (default) — WASM 1.0

| Proposal           | wazero constant              | Wasmtime |
| ------------------ | ---------------------------- | -------- |
| `mutable-globals`  | `CoreFeatureMutableGlobal`   | T1       |

### Bundled in `CoreFeaturesV2` (default) — adds WASM 2.0

| Proposal                  | wazero constant                              | Wasmtime |
| ------------------------- | -------------------------------------------- | -------- |
| `bulk-memory-operations`  | `CoreFeatureBulkMemoryOperations`            | T1       |
| `multi-value`             | `CoreFeatureMultiValue`                      | T1       |
| `nontrapping-fptoint`     | `CoreFeatureNonTrappingFloatToIntConversion` | T1       |
| `reference-types`         | `CoreFeatureReferenceTypes`                  | T1       |
| `sign-extension-ops`      | `CoreFeatureSignExtensionOps`                | T1       |
| `simd` (simd128)          | `CoreFeatureSIMD`                            | T1       |

`CoreFeaturesV2` is the default in `RuntimeConfig.WithCoreFeatures` and
is what `component.go` ends up with implicitly.

### Experimental (off by default)

Available via the `experimental` package as bit flags above
`CoreFeatureSIMD`, opt-in by OR-ing into
`RuntimeConfig.WithCoreFeatures`:

| Proposal              | wazero constant                        | Wasmtime | Notes                                                                |
| --------------------- | -------------------------------------- | -------- | -------------------------------------------------------------------- |
| `threads`             | `CoreFeaturesThreads`                  | T2       | Atomics implemented for guest-only; systems without mmap pre-allocate to max memory. |
| `tail-call`           | `CoreFeaturesTailCall`                 | T1       | —                                                                    |
| `extended-const`      | `CoreFeaturesExtendedConst`            | T1       | Arithmetic + global refs in const exprs. Added in v1.12.0.           |
| `exception-handling`  | `CoreFeaturesExceptionHandling`        | T1       | Added in v1.12.0.                                                    |
| `function-references` | `CoreFeaturesTypedFunctionReferences`  | T1       | Added in v1.12.0. Named "typed references" upstream.                 |

### Not implemented

Anything not in the three tables above. Notably absent versus
Wasmtime: `relaxed-simd`, `multi-memory`, `memory64`, `gc`,
`custom-page-sizes`, `wide-arithmetic`, `stack-switching`, and the
entire **component model**.

Also unsupported, and worth naming because Zig *can* emit it: `fp16`.
Wasmtime doesn't implement it either — it's only phase 2 — so this one
isn't a wazero gap so much as a way to build a module no host will
run. See [`../zig/WASM.md`](../zig/WASM.md).

`branch-hinting` is a special case: it rides in a custom section, and
wazero skips custom sections it doesn't recognize (it only
special-cases `name`), so a branch-hinted module still loads — it just
runs without the hints.

### Where that leaves wazero against Wasm 3.0

Core spec 3.0 bundles nine proposals. wazero has four of them behind
experimental flags (`tail-call`, `extended-const`,
`exception-handling`, `function-references`), no-ops the fifth
(`branch-hinting`), and does not implement the remaining four
(`relaxed-simd`, `multi-memory`, `gc`, `memory64`). So **out of the
box wazero is a 2.0 runtime**, and even fully opted-in it is not a 3.0
one. v1.12.0's release notes frame this as ongoing work — it
"advances us a few more steps towards Wasm 3.0".

## WASI support

| Revision      | wazero | Notes                                                    |
| ------------- | ------ | -------------------------------------------------------- |
| Preview 0     | ❌     | Historical, intentionally not supported.                 |
| **Preview 1** | ✅     | Full implementation in `imports/wasi_snapshot_preview1`. |
| Preview 2     | ❌     | Requires component model (see above).                    |
| Preview 3     | ❌     | Requires component model plus its async primitives (`async func`, `stream<T>`, `future<T>`). |

A handful of preview1 functions are tracked as in-progress; the
matrix on [wazero.io/specs](https://wazero.io/specs/) is authoritative.

The component-model gap is the load-bearing one for this project's
roadmap — it's what keeps `interface/otel.wit` aspirational. Two ways
out, neither of them "wait for wazero": swap the runtime, or transpile
components down to wasip1 so wazero can run them (arcjet/gravity).
`docs/v2_plan.md` parks the latter as future work — it would unblock
the Component Model migration without giving up pure Go.

## What this means for `wasm4otel` plugins

The features a plugin can rely on, in practice:

1. Anything in `CoreFeaturesV2` — i.e. all of WASM 2.0, `simd128`
   included.
2. WASI Preview 1, including reactor mode (`_initialize`).
3. Optionally, the five experimental proposals above — only if the
   host runtime is built with
   `wazero.NewRuntimeConfigInterpreter().WithCoreFeatures(...)`
   opting them in. Plugins should not assume any of these are
   available unless the host has been configured to provide them.

Things to **not** rely on in plugins:

- Component model, WIT bindings — would need a different runtime or a
  transpilation step.
- Multi-memory, memory64, GC, relaxed-simd, fp16, wide-arithmetic.

## Determinism defaults

Wazero is reproducible-by-default: anything that would otherwise vary
across runs (clocks, sleeps, randomness) returns a fixed value unless
the embedder explicitly opts in. This is great for tests and bad for
plugins that actually want to read wall-clock time.

The defaults a plugin sees out of the box:

| WASIp1 call                 | wazero default                               |
| --------------------------- | -------------------------------------------- |
| `clock_time_get(realtime)`  | `1640995200s, 0ns` — `2022-01-01T00:00:00Z`. |
| `clock_time_get(monotonic)` | A counter incremented by 1 ns per call.      |
| `poll_oneoff` (sleep)       | Returns immediately (no real sleep).         |
| `random_get`                | Deterministic stream from a fixed seed.      |

The opt-in toggles on `wazero.NewModuleConfig()`:

| Method                      | Effect                                                                      |
| --------------------------- | --------------------------------------------------------------------------- |
| `WithSysWalltime()`         | `realtime` clock returns the host's `time.Now()`.                           |
| `WithSysNanotime()`         | `monotonic` clock returns real `time.Now().UnixNano()`-derived nanoseconds. |
| `WithSysNanosleep()`        | `poll_oneoff` actually sleeps for the requested duration.                   |
| `WithRandSource(io.Reader)` | Replaces the random source. Pass `crypto/rand.Reader` for unpredictability. |

`LoadPlugin` wires only the first two, alongside
`WithStartFunctions("_start", "_initialize")`. That is deliberate for
sleep — plugins pace themselves through the `interruptible_sleep_ms`
host import, which the host can cancel on shutdown, rather than
through a WASI sleep it has no handle on. It is **not** deliberate for
randomness: `WithRandSource` is simply unwired, so any plugin calling
`random_get` gets the same bytes on every run.

Common symptoms:

- `time_unix_nano` stuck at `1640995200000000000` (2022-01-01) →
  `WithSysWalltime()` is not wired. (It is, in `LoadPlugin` — so this
  points at a `ModuleConfig` built somewhere else.)
- Trace IDs / UUIDs collide across collector restarts → expected
  today; `random_get` is deterministic until someone wires
  `WithRandSource(crypto/rand.Reader)`.
- A plugin's "every N seconds" loop returns immediately instead of
  pacing → it's calling a WASI sleep rather than
  `interruptible_sleep_ms`.

## Bumping wazero

The v1.11.0 → v1.12.0 bump (this repo moved on 2026-06-17) **did**
unlock new proposals, unlike the v1.10.x → v1.11.0 bump before it:
`extended-const`, `exception-handling`, and typed function references
all arrived in v1.12.0, taking the experimental set from two entries
to five. It also raised the Go floor to **1.25** — wazero tracks
"latest minus one", so expect the floor to move on most minor bumps.

Re-check `experimental/features.go` after every bump. It's where new
proposals surface first and it's only a handful of constants long, so
the diff takes seconds to read — the v1.12.0 one was three new lines.
