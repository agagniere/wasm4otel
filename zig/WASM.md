# Zig — wasm targets and feature flags

What Zig can compile to wasm, which proposals it can opt into, and
how to express that in `build.zig`. For the master list of WASM
proposals see [`../WASM.md`](../WASM.md); for what the host runtime
actually accepts see [`../go/WAZERO.md`](../go/WAZERO.md).

Authoritative sources in the Zig tree:

- [`lib/std/Target/wasm.zig`](https://codeberg.org/ziglang/zig/src/branch/master/lib/std/Target/wasm.zig) — feature enum and CPU models.
- [`lib/std/Target.zig`](https://codeberg.org/ziglang/zig/src/branch/master/lib/std/Target.zig) — the `Os.Tag` enum and version range parsing.

## Architectures

| `cpu_arch` | Notes                                                                                              |
| ---------- | -------------------------------------------------------------------------------------------------- |
| `wasm32`   | The standard 32-bit ISA. Use this unless you have a specific reason not to.                        |
| `wasm64`   | 64-bit memory addressing (the `memory64` proposal lifted into a separate ISA). Limited host support. |

## OS tags

| `os_tag`      | Standard library | Use case                                                       |
| ------------- | ---------------- | -------------------------------------------------------------- |
| `freestanding` | Minimal — no syscalls, no allocator, no I/O via the OS layer. | Embedded inside a host that grants its own imports (this repo's `freestanding/` plugins). |
| `wasi`        | `wasi_snapshot_preview1` calls.                              | Anything wanting files/clocks/random and broad runtime support. |
| `emscripten`  | Emscripten libc + JS glue.                                  | Browser-targeted, with the Emscripten toolchain on top.         |

There's a single `wasi` tag covering all WASI revisions, with the
revision *nominally* expressed via `os_version_min` / `os_version_max`
semver:

| WASI revision | semver range |
| ------------- | ------------ |
| Preview 1     | `0.1.x`      |
| Preview 2     | `0.2.x`      |
| Preview 3     | `0.3.x`      |

Treat that as documentation, not a switch. On Zig 0.16 nothing in the
stdlib branches on the WASI version range, so setting it changes no
emitted code — it only makes the target triple self-describing
(`wasm32-wasi.0.1` in `--summary all` output). `build.zig` pins it for
exactly that reason, and says so in a comment.

Nor is there anything to switch *to*: Zig's stdlib implements Preview 1
only. There is no `wasip2` / `preview2` / component-model support
anywhere in `std` on 0.16. Preview 2 and 3 are reachable only by
importing component-model bindings by hand — the language permits it,
but you write all the glue.

## Feature flags

On Zig 0.16 — the version `build.zig.zon` pins as the minimum —
`std.Target.wasm.Feature` lists 18 features. Each maps onto a
WebAssembly proposal name:

| Zig feature                    | WASM proposal                                  | Spec version        |
| ------------------------------ | ---------------------------------------------- | ------------------- |
| `mutable_globals`              | `mutable-globals`                              | 1.0                 |
| `sign_ext`                     | `sign-extension-ops`                           | 2.0                 |
| `nontrapping_fptoint`          | `nontrapping-fptoint`                          | 2.0                 |
| `multivalue`                   | `multi-value`                                  | 2.0                 |
| `bulk_memory`                  | `bulk-memory-operations`                       | 2.0                 |
| `bulk_memory_opt`              | `bulk-memory-operations` (subset of opcodes)   | 2.0                 |
| `nontrapping_bulk_memory_len0` | `bulk-memory-operations` (zero-length variant) | 2.0                 |
| `reference_types`              | `reference-types`                              | 2.0                 |
| `simd128`                      | `simd`                                         | 2.0                 |
| `relaxed_simd`                 | `relaxed-simd`                                 | 3.0                 |
| `multimemory`                  | `multi-memory`                                 | 3.0                 |
| `tail_call`                    | `tail-call`                                    | 3.0                 |
| `extended_const`               | `extended-const`                               | 3.0                 |
| `exception_handling`           | `exception-handling`                           | 3.0                 |
| `atomics`                      | `threads`                                      | phase 4, unreleased |
| `wide_arithmetic`              | `wide-arithmetic`                              | phase 4, unreleased |
| `fp16`                         | `half-precision` (FP16)                        | phase 2             |
| `call_indirect_overlong`       | (LLVM encoding tweak — no spec proposal)       | —                   |

Notably **absent** from Zig — there's no way to emit code that uses
these from Zig today: `gc`, `function-references`, `branch-hinting`,
`memory64` (use the `wasm64` ISA instead), `custom-page-sizes`,
`stack-switching`, `flexible-vectors`, `memory-control`,
`shared-everything-threads`, and the entire **component model**.

Note that the first three of those are part of core spec 3.0, so Zig
cannot emit all of the current core spec — and `fp16` runs the other
way, a feature Zig emits that no major runtime implements (it's only
phase 2, unimplemented in Wasmtime as well as wazero). See
[`../WASM.md`](../WASM.md) for where each proposal actually stands.

**On master (checked against 0.17.0-dev.2131+d08989840):** a 19th
feature, `gc`, joins the enum and the `bleeding_edge` model. Treat it
as a target flag rather than a new capability — it maps to LLVM's
`+gc` and the compiler accepts `-mcpu=generic+gc`, but there is no
wasm-GC type surface anywhere in std (no `structref`, `arrayref`,
`struct.new`, …), so there's still nothing to write in Zig that would
emit a GC instruction. `mvp`, `generic` and `lime1` are unchanged, so
this repo's target is unaffected. Nothing else in the tables above
moved: still Preview 1 only, and still no synthesized entry symbol.

## CPU models

Bundled feature sets, picked via `cpu_model`:

| Model           | Includes                                                                                                                                                  | Use when                                                                          |
| --------------- | --------------------------------------------------------------------------------------------------------------------------------------------------------- | --------------------------------------------------------------------------------- |
| `mvp`           | (nothing)                                                                                                                                                 | Maximum portability; any wasm host since 2017 will run it.                        |
| `generic`       | `bulk_memory`, `multivalue`, `mutable_globals`, `nontrapping_fptoint`, `reference_types`, `sign_ext`                                                      | Roughly WASM 2.0 minus SIMD. Safe default for a "modern" runtime.                 |
| `lime1`         | `bulk_memory_opt`, `call_indirect_overlong`, `extended_const`, `multivalue`, `mutable_globals`, `nontrapping_fptoint`, `sign_ext`                         | Tuned for "lightweight" runtimes (this repo uses it for the wasip1 plugins, since wazero advertises a matching profile). |
| `bleeding_edge` | `atomics`, `bulk_memory`, `exception_handling`, `extended_const`, `fp16`, `multimemory`, `multivalue`, `mutable_globals`, `nontrapping_fptoint`, `reference_types`, `relaxed_simd`, `sign_ext`, `simd128`, `tail_call` | Host known to be Wasmtime / V8 / SpiderMonkey on a recent version.                |

You can also start from a model and add features:

```zig
const wasip1 = b.resolveTargetQuery(.{
    .cpu_arch = .wasm32,
    .os_tag = .wasi,
    .cpu_model = .{ .explicit = &std.Target.wasm.cpu.lime1 },
    .cpu_features_add = std.Target.wasm.featureSet(&.{
        .bulk_memory, .reference_types, .simd128,
    }),
});
```

(This is exactly what `build.zig` does for wasip1 plugins — it
extends `lime1` to match wazero's full default feature set so the
output runs without opt-in flags on the host side.)

## Cross-checking against the host

The compatibility rule with the runtime: a `.wasm` module loads if and
only if every feature it actually uses is one the host has enabled.

For wazero (this repo's host), features safe to use today:

- Anything in Zig's `generic` model.
- Add `simd128` if you need it (wazero `CoreFeatureSIMD` is on by default).

Features Zig *can* emit but wazero won't run without changes to
`go/component.go`: `tail_call`, `extended_const`, `atomics`,
`exception_handling`. These are four of wazero's five experimental
opt-ins — the fifth, `function-references`, Zig has no way to emit, so
the two toolchains meet on exactly four negotiable features.

Features Zig can emit and wazero will never accept (until it gains
support): `relaxed_simd`, `multimemory`, `fp16`, `wide_arithmetic`.
`fp16` is the bleakest of those — no major runtime implements it,
Wasmtime included.

`bleeding_edge` is **not** safe against the current wazero
configuration — it pulls in `relaxed_simd`, `multimemory`, and
`fp16` which wazero rejects.

## Reactor vs command

Independent of features, WASI plugins pick an execution model on the
exe:

```zig
exe.wasi_exec_model = .reactor;   // entry symbol is _initialize, exports stay live
// or
exe.wasi_exec_model = .command;   // entry symbol is _start, exits when done
```

The field is `?std.builtin.WasiExecModel`, and `build.zig` leaves it
`null` for the freestanding target — the flag is only passed to the
compiler when set, and it means nothing outside `os_tag = .wasi`.

`wasm4otel`'s wasip1 plugins are reactors, because the host calls
`start()` / `stop()` on lifecycle events rather than running the
module top-to-bottom once.

**Setting `wasi_exec_model` does not create the entry symbol.** On Zig
0.16 the toolchain synthesizes neither `_initialize` nor `_start`; the
plugin has to export one itself:

```zig
comptime {
    @export(&init, .{ .name = "_initialize" });
}

fn init() callconv(.{ .wasm_mvp = .{} }) void { ... }
```

Every plugin in this repo does this — `_initialize` in `wasip1/`,
`_start` in `freestanding/` (see `wasip1/log_generator.zig:20-22`).
The freestanding ones have no execution model at all; `_start` there is
just an export name the host happens to call, not a WASI concept. The
host asks for both names —
`WithStartFunctions("_start", "_initialize")` in `go/component.go` —
so either spelling works from its side, but one of them must exist.
Omit both and nothing complains: wazero skips a start function that
isn't exported rather than erroring, so the module instantiates
successfully and is simply never initialized. That surfaces much later
as an uninitialized allocator or a null global, far from the cause.
Export only one name, too — wazero calls every start function it
finds, so exporting both runs your init twice.
