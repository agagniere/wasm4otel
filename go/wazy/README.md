# `go/wazy/` — collector host on wazy

Go module: `github.com/agagniere/wasm4otel/go/wazy`.

The same three OpenTelemetry Collector components as
[`go/wazero`](../wazero/README.md) — receiver, processor, exporter —
built on [wazy](https://github.com/samyfodil/wazy) instead of
[wazero](https://github.com/tetratelabs/wazero). Both modules speak the
same plugin ABI, so the same plugin runs under either without a
rebuild.

The two are separate Go modules and a collector build should import
**one** of them. Nothing shares code between them: `component.go` is
duplicated on purpose, and kept line-for-line close to
[`../wazero/component.go`](../wazero/component.go) so `diff` shows only
the runtime-specific parts. Read the
[wazero README](../wazero/README.md) for the ABI, the config schema,
the lifecycle and the host imports — all of it applies verbatim. Its
[`ocb` manifest section](../wazero/README.md#adding-the-components-to-an-ocb-manifest)
applies too, with `go/wazero` swapped for `go/wazy` in the `gomod` and
`import` fields. This document covers only what is different here.

## Why a second host

wazy is a fork of wazero that supports **WASI 0.2 and the Component
Model** natively (`imports/wasip2`, plus a `component` package),
alongside core Wasm 1.0/2.0/3.0 and WASI 0.1. It is pure Go with no
CGO, same as wazero.

That matters because the Component Model is where this plugin ABI wants
to end up — see [`interface/`](../../interface) for the ABI restated as
WIT, and the reasons the Canonical ABI deletes `wasm4otel_alloc`,
`wasm4otel_free` and the `get_config` probe dance outright. wazero has
no component support today, so porting the host is the step that
unblocks trying any of it.

Nothing in this module uses components yet. It loads the same core
modules the wazero host does, over wazy's core-module API. That is
deliberate: it isolates "does the host port cleanly?" from "is the WIT
the right shape?", and it means this module can be swapped in and the
existing plugins keep working.

## What differs from the wazero host

Four things, all in `component.go`:

### Host imports are registered with typed generics

wazy deliberately dropped wazero's reflection-based
`NewFunctionBuilder().WithFunc(fn)`, which derives a wasm signature
from a Go function's type at *runtime*. In its place are
compile-time-typed helpers, one per arity:

```go
builder := self.runtime.NewHostModuleBuilder("env")
wazy.HostProc3(builder.NewFunctionBuilder(), self.logToZap).Export("host_log")
wazy.HostFunc2(builder.NewFunctionBuilder(), self.outboundLogs).Export("push_logs")
// ...
_, err := builder.Instantiate(self.context)
```

`HostFuncN` is for a handler returning a value, `HostProcN` for one
returning nothing; `N` counts the wasm-level parameters. A mismatched
arity or an unrepresentable parameter type is a **compile error** here,
where under wazero the equivalent mistake panics when the host module
is instantiated. That is the main practical win of the port.

The helpers require `api.Module` as the second parameter, so
`interruptibleSleepMs` carries one it never uses — the wazero version
omits it. `HostValue` also accepts only the exact predeclared numeric
types, so a named type like `type Level int32` will not do; pass the
underlying type and convert inside the handler.

### Instantiation failures come back as errors

The wazero host prints a `proc_exit` code to stderr and calls
`logger.Panicln` for any other instantiation failure. Here both travel
out through `Load` as a wrapped error naming the plugin path — a bad
plugin file is the operator's problem, not grounds for panicking the
collector. The `*sys.ExitError` is matched with `errors.As` rather than
a type assertion so a wrapped one is still recognised.

### `Shutdown` closes the runtime

After `wasm4otel_shutdown` returns, this host calls
`runtime.Close(ctx)`, releasing the instance and everything the runtime
compiled for it. Safe even after a trap: closing is the one operation a
poisoned instance still owes us. The wazero host leaks this today.

### Import list and ABI: unchanged

`env` still exports `host_log`, `push_logs`, `push_metrics`,
`push_traces`, `interruptible_sleep_ms` and `get_config`, with
identical signatures, return codes and semantics. The guest-side export
names, the role checks, the return-code enums and the per-batch
`alloc → write → call → free` sequence are all the same. A plugin
cannot tell which host it is running under.

## Tests

This is the ABI conformance suite, and it is not wazy's alone: the
file is byte-identical to [`go/wazero`](../wazero/README.md)'s copy and
both modules run it against the same fixtures. Running one suite
against both hosts is what checks the interchangeability the rest of
this document claims — a plugin cannot tell the hosts apart only if
the same tests pass under both. `go test -race ./...` covers the
lifecycle end to end:

| Test                            | What it pins                                                                                                   |
| ------------------------------- | -------------------------------------------------------------------------------------------------------------- |
| `TestProcessorRoundTrip`        | A batch survives marshal → alloc → memory write → `wasm4otel_process_logs` → `push_logs` → the returned batch. |
| `TestExporterIsTerminal`        | Exporter mode routes to `wasm4otel_export_logs`, and tolerates no downstream consumer.                         |
| `TestReceiveLoopRunsAndStops`   | `Start` spawns `wasm4otel_receive` (it pushes a batch, so the sink proves it ran); `Shutdown` joins it.        |
| `TestModeExportValidation`      | A processor-only plugin is rejected under `receivers:`.                                                        |
| `TestSignalExportValidation`    | Per-signal, per-role export checks name the export that is missing.                                            |
| `TestRoleChecksAreAdditive`     | A multi-role plugin is accepted in every section it exports for — the checks never assert absence.             |
| `TestSetupRejectsBadConfig`     | `wasm4otel_setup` returning `2` fails `Load`, i.e. the collector refuses to boot.                              |
| `TestGetConfigProbeThenRead`    | The `(0, 0)` probe, the undersized-buffer refusal, and the sized read.                                         |
| `TestPoisonedAfterTrap`         | Once `broken` latches, further guest entries are refused rather than compounding corruption.                   |

The fixtures in [`../testdata/`](../testdata) are hand-written WAT
rather than compiled Zig, so the suite needs no toolchain beyond Go.
They sit above both modules because both run them: a fixture change
lands on the two hosts at once, which is what keeps them honest. The
`.wasm` files are committed; regenerate them after editing a `.wat`
with:

```sh
make -C ../testdata
```

which needs [wasm-tools](https://github.com/bytecodealliance/wasm-tools).

`full_plugin.wat` exports every role at once, which is what makes the
additive-role-check test meaningful. It is the WAT counterpart of
[`zig/freestanding/helloworld.zig`](../../zig/freestanding/helloworld.zig) —
the duplication is deliberate, so the suite runs with no Zig toolchain
installed.

## Dependencies

Pinned in `go.mod` — identical to the wazero module except for the
runtime:

- `github.com/samyfodil/wazy` — wasm runtime.
- `go.opentelemetry.io/collector/{component,consumer,pdata,receiver,processor,exporter}`
- `go.uber.org/zap`

## Building and testing

```sh
go build ./...
go test -race ./...
```
