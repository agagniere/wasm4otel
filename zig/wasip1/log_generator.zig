const std = @import("std");
const builtin = @import("builtin");
const build_info = @import("build_info");
const otelData = @import("otel_pipeline_data");
const host_log = @import("hostlog");

const Allocator = std.mem.Allocator;
const Io = std.Io;
const LogsBatch = otelData.Logs.LogsData;

pub const std_options: std.Options = .{
    .logFn = host_log.logFn,
    .log_level = .debug,
};

/// Use to forward log batches to the next consumer in the OTel pipeline
extern fn push_logs(ptr: [*]const u8, size: usize) i32;

comptime {
    @export(&init, .{ .name = "_initialize" });
}

/// Entry point of this plugin
fn init() callconv(.{ .wasm_mvp = .{} }) void {
    std.log.info("Initializing {s}, part of {s} version {s}, built with Zig {s}", .{
        @src().file,
        build_info.name,
        build_info.version,
        builtin.zig_version_string,
    });
    std.log.info("Running on {t} {t} {t} {t}", .{
        builtin.cpu.arch,
        builtin.os.tag,
        builtin.abi,
        builtin.wasi_exec_model,
    });
}

/// Begin logs reception
export fn start() void {
    const alloc: Allocator = std.heap.wasm_allocator;
    var threaded: Io.Threaded = .init_single_threaded;
    const io = threaded.io();

    const logs = generateLogs(alloc, io) catch |err| {
        std.log.err("Fail generation: {t}", .{err});
        return;
    };
    pushLogs(alloc, logs) catch {
        std.log.err("Failed to send", .{});
        return;
    };
}

/// End logs reception
export fn stop() void {
    std.log.info("So long !", .{});
}

/// Push an encoded batch of logs to the next consumer
fn pushSerializedLogs(logs: []const u8) bool {
    return push_logs(logs.ptr, logs.len) == 0;
}

/// Push a batch of logs to the next consumer
fn pushLogs(alloc: Allocator, logs: LogsBatch) !void {
    var buffer: [2048]u8 = undefined;
    var writer: Io.Writer = .fixed(&buffer);

    try logs.encode(&writer, alloc);
    std.log.info("Sending {} bytes of logs", .{writer.buffered().len});
    _ = pushSerializedLogs(writer.buffered());
}

fn generateLogs(alloc: Allocator, io: Io) !LogsBatch {
    var batch: LogsBatch = .{};

    var logs_from_host = try batch.resource_logs.addOne(alloc);
    logs_from_host.* = .{ .resource = .{} };
    try logs_from_host.resource.?.attributes.append(alloc, .{ .key = "service.name", .value = .{ .value = .{ .string_value = build_info.name } } });
    try logs_from_host.resource.?.attributes.append(alloc, .{ .key = "service.version", .value = .{ .value = .{ .string_value = build_info.version } } });

    const location = @src();
    var logs_from_instance = try logs_from_host.scope_logs.addOne(alloc);
    logs_from_instance.* = .{ .scope = .{ .name = location.fn_name, .version = build_info.version } };
    try logs_from_instance.scope.?.attributes.append(alloc, .{ .key = "file", .value = .{ .value = .{ .string_value = location.file } } });

    for (0..6) |i| {
        const now: u64 = @intCast(std.Io.Clock.now(.real, io).toNanoseconds());
        const log: otelData.Logs.LogRecord = .{
            .time_unix_nano = now,
            .observed_time_unix_nano = now,
            .severity_number = @enumFromInt((i % 6) * 4 + 1),
            .body = .{ .value = .{ .string_value = "Hello from WebAssembly" } },
        };
        try logs_from_instance.log_records.append(alloc, log);
    }
    return batch;
}
