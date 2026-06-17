const std = @import("std");
const builtin = @import("builtin");
const build_info = @import("build_info");
const otelData = @import("otel_pipeline_data");
const guest = @import("guest");
const host = @import("host");

const Allocator = std.mem.Allocator;
const Io = std.Io;
const LogsBatch = otelData.Logs.LogsData;

pub const std_options: std.Options = .{
    .logFn = host.logFn,
    .log_level = .info,
};

/// Forwarding path to the next consumer in the OTel pipeline.
extern fn push_logs(ptr: [*]const u8, size: usize) i32;

comptime {
    @export(&init, .{ .name = "_start" });
    @export(&guest.alloc, .{ .name = "wasm4otel_alloc" });
    @export(&guest.free, .{ .name = "wasm4otel_free" });
}

/// Called by wazero at instantiation time.
fn init() callconv(.{ .wasm_mvp = .{} }) void {
    std.log.info("{s} v{s} loaded — dropping log records below severity INFO", .{
        build_info.name,
        build_info.version,
    });
}

/// Severity numbers as defined by OTLP. Records strictly below
/// `min_severity` are dropped.
const min_severity: i32 = 9; // SEVERITY_NUMBER_INFO

/// Host calls this with `(ptr, size)` referring to a buffer it just
/// wrote into our linear memory via `wasm4otel_alloc`. Decode,
/// filter, re-encode, forward via `push_logs`. The host calls
/// `wasm4otel_free` as soon as we return — do not retain the pointer.
export fn consume_logs(ptr: [*]const u8, size: usize) i32 {
    var arena: std.heap.ArenaAllocator = .init(std.heap.wasm_allocator);
    defer arena.deinit();
    const alloc = arena.allocator();

    const payload = ptr[0..size];
    var reader: Io.Reader = .fixed(payload);

    var batch = LogsBatch.decode(&reader, alloc) catch |err| {
        std.log.err("decode failed: {t}", .{err});
        return 1;
    };

    filterInPlace(&batch);

    var out: Io.Writer.Allocating = .init(alloc);
    defer out.deinit();
    batch.encode(&out.writer, alloc) catch |err| {
        std.log.err("encode failed: {t}", .{err});
        return 2;
    };

    const bytes = out.written();
    return push_logs(bytes.ptr, bytes.len);
}

fn filterInPlace(batch: *LogsBatch) void {
    for (batch.resource_logs.items) |*rl| {
        for (rl.scope_logs.items) |*sl| {
            var write: usize = 0;
            for (sl.log_records.items) |record| {
                if (@intFromEnum(record.severity_number) >= min_severity) {
                    sl.log_records.items[write] = record;
                    write += 1;
                }
            }
            sl.log_records.shrinkRetainingCapacity(write);
        }
    }
}
