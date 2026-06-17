const std = @import("std");
const builtin = @import("builtin");
const build_info = @import("build_info");

pub const std_options: std.Options = .{
    .logFn = @import("host").logFn,
    .log_level = .debug,
};

comptime {
    @export(&init, .{ .name = "_start" });
}

/// Called when loaded
fn init() callconv(.{ .wasm_mvp = .{} }) void {
    std.log.info("Hello from {t} {t} {t}", .{
        builtin.cpu.arch,
        builtin.os.tag,
        builtin.abi,
    });
}

export fn start() void {
    std.log.info("Starting {s}, part of {s} version {s}, built with Zig {s}", .{
        @src().file,
        build_info.name,
        build_info.version,
        builtin.zig_version_string,
    });
}

export fn stop() void {
    std.log.info("So long !", .{});
}
