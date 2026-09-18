const std = @import("std");
const builtin = @import("builtin");
const build_info = @import("build_info");
const guest = @import("guest");

pub const std_options: std.Options = .{
    .logFn = @import("host").logFn,
    .log_level = .debug,
};

// The plugin's whole ABI surface, in the order the host calls it.
// Every export v2 defines is here, so this one module is loadable in
// every role: receiver, processor on any signal, exporter on any
// signal. That makes it the reference for *which names the host looks
// up* — not a useful pipeline component, since nothing here pushes or
// forwards telemetry.
comptime {
    @export(&init, .{ .name = "_start" });
    @export(&setup, .{ .name = "wasm4otel_setup" });
    @export(&start, .{ .name = "wasm4otel_start" });
    @export(&receive, .{ .name = "wasm4otel_receive" });
    @export(&shutdown, .{ .name = "wasm4otel_shutdown" });

    // Processor and exporter differ only in whether the plugin hands
    // its result to the next consumer, and a no-op hands on nothing
    // either way — so one function per signal covers both names.
    @export(&handleLogs, .{ .name = "wasm4otel_process_logs" });
    @export(&handleLogs, .{ .name = "wasm4otel_export_logs" });
    @export(&handleMetrics, .{ .name = "wasm4otel_process_metrics" });
    @export(&handleMetrics, .{ .name = "wasm4otel_export_metrics" });
    @export(&handleTraces, .{ .name = "wasm4otel_process_traces" });
    @export(&handleTraces, .{ .name = "wasm4otel_export_traces" });

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
    std.log.info("So long !", .{});
}

fn handleLogs(ptr: [*]const u8, size: usize) callconv(.{ .wasm_mvp = .{} }) i32 {
    return dropBatch("logs", ptr, size);
}

fn handleMetrics(ptr: [*]const u8, size: usize) callconv(.{ .wasm_mvp = .{} }) i32 {
    return dropBatch("metrics", ptr, size);
}

fn handleTraces(ptr: [*]const u8, size: usize) callconv(.{ .wasm_mvp = .{} }) i32 {
    return dropBatch("traces", ptr, size);
}

/// `ptr[0..size]` is the encoded OTLP batch the host wrote into our
/// linear memory; it calls `wasm4otel_free` as soon as we return, so
/// the pointer must not be retained. Returning 0 without forwarding
/// means the batch ends here — wire this plugin as a processor and it
/// will quietly swallow the pipeline, which is why it's a shape demo
/// and not something to deploy.
fn dropBatch(signal: []const u8, ptr: [*]const u8, size: usize) i32 {
    _ = ptr;
    std.log.info("Dropping a {d} byte batch of {s}", .{ size, signal });
    return 0;
}
