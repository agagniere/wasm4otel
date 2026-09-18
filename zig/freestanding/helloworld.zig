const std = @import("std");
const builtin = @import("builtin");
const build_info = @import("build_info");
const guest = @import("guest");

pub const std_options: std.Options = .{
    .logFn = @import("host").logFn,
    .log_level = .debug,
};

/// Forwarding paths to the next consumer in the OTel pipeline.
extern fn push_logs(ptr: [*]const u8, size: usize) i32;
extern fn push_metrics(ptr: [*]const u8, size: usize) i32;
extern fn push_traces(ptr: [*]const u8, size: usize) i32;

// The plugin's whole ABI surface, in the order the host calls it.
// Nothing is left out, so this one module is loadable in every role:
// receiver, processor on any signal, exporter on any signal. That
// makes it the reference for *which names the host looks up* — and,
// since its batch paths do nothing but move bytes, a baseline for
// what the wasm hop alone costs.
comptime {
    @export(&init, .{ .name = "_start" });
    @export(&setup, .{ .name = "wasm4otel_setup" });
    @export(&start, .{ .name = "wasm4otel_start" });
    @export(&receive, .{ .name = "wasm4otel_receive" });
    @export(&shutdown, .{ .name = "wasm4otel_shutdown" });

    // The two halves of the batch ABI differ in exactly the thing
    // these do: `process_` hands the bytes on through `push_<signal>`,
    // `export_` is terminal. Each `process_` name needs its own
    // function — a different import each — while dropping a batch is
    // signal-agnostic, so one function answers to all three `export_`
    // names.
    @export(&processLogs, .{ .name = "wasm4otel_process_logs" });
    @export(&processMetrics, .{ .name = "wasm4otel_process_metrics" });
    @export(&processTraces, .{ .name = "wasm4otel_process_traces" });
    @export(&dropBatch, .{ .name = "wasm4otel_export_logs" });
    @export(&dropBatch, .{ .name = "wasm4otel_export_metrics" });
    @export(&dropBatch, .{ .name = "wasm4otel_export_traces" });

    // The real implementations, not stubs: the host writes each batch
    // into the buffer `wasm4otel_alloc` hands back, so an allocator
    // that always returned 0 would point it at address 0 and let it
    // scribble over our data segment.
    @export(&guest.alloc, .{ .name = "wasm4otel_alloc" });
    @export(&guest.free, .{ .name = "wasm4otel_free" });
}

/// Called when loaded
fn init() callconv(.{ .wasm_mvp = .{} }) void {
    std.log.info("Hello from {t} {t} {t}", .{
        builtin.cpu.arch,
        builtin.os.tag,
        builtin.abi,
    });
}

/// Runs inside the factory, before the collector finishes booting —
/// where a plugin reads and validates its YAML `plugin_config` and
/// returns `.invalid_config` to refuse the boot. Nothing to validate
/// here, so: success.
fn setup() callconv(.{ .wasm_mvp = .{} }) guest.SetupResult {
    return .success;
}

/// The short-lived init hook every role gets, called synchronously at
/// `Component.Start`. Declared as `()` rather than `i32` to show that
/// shape works too — the host reads an empty result as success.
fn start() callconv(.{ .wasm_mvp = .{} }) void {
    std.log.info("Starting {s}, part of {s} version {s}, built with Zig {s}", .{
        @src().file,
        build_info.name,
        build_info.version,
        builtin.zig_version_string,
    });
}

/// The receiver's loop, run on its own goroutine. A real receiver
/// stays in here calling `push_logs` / `push_metrics` / `push_traces`
/// until shutdown cancels it; this one returns immediately, so the
/// host's goroutine simply drains. Unlike `start` above it returns an
/// explicit `StartResult` — both shapes are valid for either export.
fn receive() callconv(.{ .wasm_mvp = .{} }) guest.StartResult {
    std.log.info("Nothing to receive, returning right away", .{});
    return .success;
}

fn shutdown() callconv(.{ .wasm_mvp = .{} }) void {
    std.log.info("So long ! Handled {d} batches ({d} bytes)", .{ batches, bytes });
}

/// What the batch paths saw, reported once by `shutdown`. A `host_log`
/// per batch would cost several times the ABI hop it is meant to
/// measure, so the batch paths only bump these counters and stay
/// quiet. Plain globals are enough: linear memory belongs to one
/// instance, and the host serializes every call into it.
var batches: u64 = 0;
var bytes: u64 = 0;

/// Hands the batch straight back to the host, byte for byte:
/// `ptr[0..size]` is still the OTLP payload the host wrote into our
/// linear memory, and `push_logs` reads it from there without this
/// side ever decoding it. Running this as a processor therefore costs
/// what the wasm hop costs and nothing else — the host's marshal, the
/// copy into linear memory, the call, then the read back out and
/// unmarshal — which is the baseline a real processor's own work is
/// measured against. `severity_filter.zig` is this same path with a
/// protobuf codec in the middle.
///
/// `push_logs` returns 0 on success and its code travels back to the
/// host unchanged, since nothing here can fail on its own.
fn processLogs(ptr: [*]const u8, size: usize) callconv(.{ .wasm_mvp = .{} }) i32 {
    count(size);
    return push_logs(ptr, size);
}

/// As `processLogs`, for a `MetricsData` batch.
fn processMetrics(ptr: [*]const u8, size: usize) callconv(.{ .wasm_mvp = .{} }) i32 {
    count(size);
    return push_metrics(ptr, size);
}

/// As `processLogs`, for a `TracesData` batch.
fn processTraces(ptr: [*]const u8, size: usize) callconv(.{ .wasm_mvp = .{} }) i32 {
    count(size);
    return push_traces(ptr, size);
}

/// The terminal half of the pair, exported under all three
/// `wasm4otel_export_<signal>` names: an exporter is the end of the
/// pipeline, so the batch stops here — dropped, in this plugin's
/// case, which is the same thing to do whatever signal it carried.
/// This is also the inbound half of the benchmark above: wired as an
/// exporter, the module measures what delivering a batch into wasm
/// costs with nothing on the way out to pay for.
fn dropBatch(ptr: [*]const u8, size: usize) callconv(.{ .wasm_mvp = .{} }) i32 {
    _ = ptr;
    count(size);
    return 0;
}

fn count(size: usize) void {
    batches += 1;
    bytes += size;
}
