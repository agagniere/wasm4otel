const std = @import("std");
const builtin = @import("builtin");

pub const std_options: std.Options = .{
    .logFn = @import("log.zig").logFn,
    .log_level = .debug,
};

/// Use to produce log batches to the next consumer in the OTel pipeline
extern fn push_logs(ptr: [*]const u8, size: usize) i32;

comptime {
    @export(&init, .{ .name = "_initialize" });
}

fn init() callconv(.{ .wasm_mvp = .{} }) void {}

export fn start() bool {
    std.log.info("Hello from {t} {t} {t}", .{
        builtin.cpu.arch,
        builtin.abi,
        builtin.wasi_exec_model,
    });
    _ = push_logs(&.{}, 0);
    return true;
}

export fn stop() bool {
    std.log.warn("So long", .{});
    return true;
}
