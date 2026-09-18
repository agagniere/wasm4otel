const std = @import("std");
const builtin = @import("builtin");
const build_info = @import("build_info");

pub const std_options: std.Options = .{
    .logFn = @import("host").logFn,
    .log_level = .debug,
};

// The plugin's whole ABI surface, in the order the host calls it.
comptime {
    @export(&init, .{ .name = "_start" });
    @export(&start, .{ .name = "wasm4otel_start" });
    @export(&shutdown, .{ .name = "wasm4otel_shutdown" });
}

/// Called when loaded
fn init() callconv(.{ .wasm_mvp = .{} }) void {
    std.log.info("Hello from {t} {t} {t}", .{
        builtin.cpu.arch,
        builtin.os.tag,
        builtin.abi,
    });
}

/// Logs once and returns, pushing no telemetry — which is exactly
/// what `wasm4otel_start` is for: the short-lived init hook every
/// role gets. `wasm4otel_receive` would be the wrong slot, that one
/// is the receiver's long-running push loop (see `one_log.zig`).
/// Returning `()` instead of an i32 is the shape the host reads as
/// success.
fn start() callconv(.{ .wasm_mvp = .{} }) void {
    std.log.info("Starting {s}, part of {s} version {s}, built with Zig {s}", .{
        @src().file,
        build_info.name,
        build_info.version,
        builtin.zig_version_string,
    });
}

fn shutdown() callconv(.{ .wasm_mvp = .{} }) void {
    std.log.info("So long !", .{});
}
