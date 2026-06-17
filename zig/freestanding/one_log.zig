const std = @import("std");
const builtin = @import("builtin");
const build_info = @import("build_info");
const otelData = @import("otel_pipeline_data");
const host = @import("host");

const Allocator = std.mem.Allocator;
const LogsBatch = otelData.Logs.LogsData;

pub const std_options: std.Options = .{
    .logFn = host.logFn,
    .log_level = .debug,
};

extern fn push_logs(ptr: [*]const u8, size: usize) i32;

comptime {
    @export(&init, .{ .name = "_start" });
}

fn init() callconv(.{ .wasm_mvp = .{} }) void {
    std.log.info("Initializing {s}, part of {s} version {s}, built with Zig {s}", .{
        @src().file,
        build_info.name,
        build_info.version,
        builtin.zig_version_string,
    });
    std.log.info("Running on {t} {t} {t}", .{
        builtin.cpu.arch,
        builtin.os.tag,
        builtin.abi,
    });
}

export fn start() void {
    std.log.debug("Generating and pushing 1 log", .{});
    pushOne(std.heap.wasm_allocator) catch |err| {
        std.log.err("Failed to push log: {t}", .{err});
    };
    std.log.debug("Success !", .{});
}

fn pushOne(alloc: Allocator) !void {
    var batch: LogsBatch = .{};

    var resource_logs = try batch.resource_logs.addOne(alloc);
    resource_logs.* = .{ .resource = .{} };
    try resource_logs.resource.?.attributes.append(alloc, .{
        .key = "service.name",
        .value = .{ .value = .{ .string_value = build_info.name } },
    });
    try resource_logs.resource.?.attributes.append(alloc, .{
        .key = "service.version",
        .value = .{ .value = .{ .string_value = build_info.version } },
    });

    var scope_logs = try resource_logs.scope_logs.addOne(alloc);
    scope_logs.* = .{ .scope = .{ .name = "freestanding", .version = build_info.version } };

    try scope_logs.log_records.append(alloc, .{
        .severity_number = @enumFromInt(9),
        .body = .{ .value = .{ .string_value = "Hello from freestanding wasm" } },
    });

    var buffer: [1024]u8 = undefined;
    var writer: std.Io.Writer = .fixed(&buffer);
    try batch.encode(&writer, alloc);

    const encoded = writer.buffered();
    std.log.info("Sending {} bytes of logs", .{encoded.len});
    _ = push_logs(encoded.ptr, encoded.len);
}
