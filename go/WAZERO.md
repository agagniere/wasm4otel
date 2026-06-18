# wazero — what we get from the host runtime

[wazero](https://github.com/tetratelabs/wazero) is the wasm runtime
embedded by `wasm4otel`'s Go side. It's the only major pure-Go wasm
runtime, which is why it fits a collector receiver: no cgo, no
platform-specific build steps, no shared library to ship.

This file covers which wasm features wazero implements, and how that
constrains the `.wasm` modules a collector can load. For the master
list of WASM proposals see [`../WASM.md`](../WASM.md); for WASI
revisions see [`../WASI.md`](../WASI.md).

## Runtime modes

wazero has two execution backends:

- **Interpreter** — pure Go, runs everywhere Go runs.
- **Compiler** — JIT-style ahead-of-time compilation to native code.
  Available on `linux/darwin/windows` × `amd64/arm64`.

`wasm4otel` currently uses the interpreter (see
[`go/README.md`](README.md) for the rationale and how to switch).
Both backends implement the same feature set; the choice is purely
performance vs portability.

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

Available via the `experimental` package, opt-in by OR-ing into
`RuntimeConfig.WithCoreFeatures`:

| Proposal             | wazero constant                | Wasmtime | Notes                                                                |
| -------------------- | ------------------------------ | -------- | -------------------------------------------------------------------- |
| `tail-call`          | `CoreFeaturesTailCall`         | T1       | —                                                                    |
| `extended-const`     | `CoreFeaturesExtendedConst`    | T1       | Arithmetic + global refs in const exprs.                             |
| `threads`            | `CoreFeaturesThreads`          | T2       | Atomics implemented for guest-only; systems without mmap pre-allocate to max memory. |
| `exception-handling` | `CoreFeaturesExceptionHandling` | T2       | —                                                                    |

### Not implemented

Anything not in the two tables above. Notably absent versus Wasmtime:
`relaxed-simd`, `multi-memory`, `memory64`, `gc`, `function-references`,
`custom-page-sizes`, `wide-arithmetic`, `stack-switching`, the entire
**component model**.

The component-model gap is the load-bearing one for this project's
roadmap: until wazero implements it, switching `wasm4otel` to a
WIT-defined ABI means swapping the runtime.

## WASI support

| Revision     | wazero | Notes                                                       |
| ------------ | ------ | ----------------------------------------------------------- |
| Preview 0    | ❌     | Historical, intentionally not supported.                    |
| **Preview 1** | ✅     | Full implementation in `imports/wasi_snapshot_preview1`.    |
| Preview 2    | ❌     | Requires component model (see above).                       |
| Preview 3    | ❌     | Requires component model + async.                           |

A handful of preview1 functions are tracked as in-progress; the
matrix on [wazero.io/specs](https://wazero.io/specs/) is authoritative.

## What this means for `wasm4otel` plugins

The features a plugin can rely on, in practice:

1. Anything in `CoreFeaturesV2` (i.e. WASM 2.0 minus relaxed-simd).
2. WASI Preview 1, including reactor mode (`_initialize`).
3. Optionally, threads/tail-call/extended-const/exception-handling —
   only if the host runtime is built with
   `wazero.NewRuntimeConfigInterpreter().WithCoreFeatures(...)`
   opting them in. Plugins should not assume any of these are
   available unless the host has been configured to provide them.

Things to **not** rely on in plugins:

- Component model, WIT bindings — would need a different runtime.
- Multi-memory, memory64, GC, function references, relaxed-simd.
- Anything Tier 3+ in Wasmtime — wazero hasn't caught up.

## Determinism defaults

Wazero is reproducible-by-default: anything that would otherwise vary
across runs (clocks, sleeps, randomness) returns a fixed value unless
the embedder explicitly opts in. This is great for tests and bad for
plugins that actually want to read wall-clock time.

The defaults a plugin sees out of the box:

| WASIp1 call             | wazero default                                    |
| ----------------------- | ------------------------------------------------- |
| `clock_time_get(realtime)`  | `1640995200s, 0ns` — `2022-01-01T00:00:00Z`. |
| `clock_time_get(monotonic)` | A counter incremented by 1 ns per call.       |
| `poll_oneoff` (sleep)   | Returns immediately (no real sleep).              |
| `random_get`            | Deterministic stream from a fixed seed.           |

Opt-in toggles on `wazero.NewModuleConfig()` (used in `LoadPlugin`):

| Method                          | Effect                                                                        |
| ------------------------------- | ----------------------------------------------------------------------------- |
| `WithSysWalltime()`             | `realtime` clock returns the host's `time.Now()`.                             |
| `WithSysNanotime()`             | `monotonic` clock returns real `time.Now().UnixNano()`-derived nanoseconds.   |
| `WithSysNanosleep()`            | `poll_oneoff` actually sleeps for the requested duration.                     |
| `WithRandSource(io.Reader)`     | Replaces the random source. Pass `crypto/rand.Reader` for unpredictability.   |

If a plugin needs real (non-deterministic) values from any of these
WASI calls, ensure the matching `With…` is set on the
`wazero.ModuleConfig` passed to `runtime.InstantiateWithConfig` (see
`component.go`). Common symptoms of a missing opt-in:

- `time_unix_nano` stuck at `1640995200000000000` (2022-01-01) →
  `WithSysWalltime()` is not wired.
- A plugin's "every N seconds" loop returns immediately instead of
  pacing → `WithSysNanosleep()` is not wired.
- Trace IDs / UUIDs collide across collector restarts →
  `WithRandSource(crypto/rand.Reader)` is not wired.

## Bumping wazero

The wazero v1.10.x → v1.11.0 bump in this repo did **not** unlock
new wasm proposals; v1.11.0's notable changes are bug fixes,
requiring Go 1.24+, and tightening platform support via
`golang.org/x/sys`. Future minor bumps may add experimental flags —
re-check `experimental/features.go` after bumping.
