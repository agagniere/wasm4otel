//! A processor that fills in `severity_number` on log records that
//! only carry a textual `severity_text` (`"INFO"`, `"warn"`, …) — the
//! usual shape when logs come from a text-based source like syslog or
//! a log file, where the level was never anything but a word.
//! Downstream components that route or filter on `severity_number`
//! (`freestanding/severity_filter.zig`, for one) can't see those
//! records otherwise.
//!
//! It doubles as the repo's example of how unit tests get wired up —
//! see the `test {}` blocks at the bottom and the `tests = true` flag
//! in `build.zig`. Run them with:
//!
//!     zig build test -fwasmtime
//!
//! `-fwasmtime` lets the build runner invoke wasmtime against the
//! compiled `wasm32-wasi` test binary.

const std = @import("std");
const builtin = @import("builtin");
const build_info = @import("build_info");
const guest = @import("guest");
const host = @import("host");
const otelData = @import("otel_pipeline_data");

const Io = std.Io;
const LogsBatch = otelData.Logs.LogsData;
const SeverityNumber = otelData.Logs.SeverityNumber;

// Under `zig build test`, the binary runs in wasmtime, which doesn't
// provide our `env.host_log` import. Fall back to std's default
// `logFn` (writes to stderr via `fd_write`, which wasmtime supports)
// when `builtin.is_test` is true; keep the host-routed logger otherwise.
pub const std_options: std.Options = if (builtin.is_test) .{
    .log_level = .debug,
} else .{
    .logFn = host.logFn,
    .log_level = .debug,
};

/// Forwarding path to the next consumer in the OTel pipeline. Only
/// reached in non-test builds — see `forward`.
extern fn push_logs(ptr: [*]const u8, size: usize) i32;

// The plugin's whole ABI surface, in the order the host calls it.
comptime {
    @export(&init, .{ .name = "_initialize" });
    @export(&start, .{ .name = "wasm4otel_start" });
    @export(&processLogs, .{ .name = "wasm4otel_process_logs" });
    @export(&guest.alloc, .{ .name = "wasm4otel_alloc" });
    @export(&guest.free, .{ .name = "wasm4otel_free" });
}

fn init() callconv(.{ .wasm_mvp = .{} }) void {
    std.log.info("Initializing {s}, part of {s} version {s}, built with Zig {s}", .{
        @src().file,
        build_info.name,
        build_info.version,
        builtin.zig_version_string,
    });
}

/// The short-lived init hook, called synchronously at
/// `Component.Start`. Nothing to open or late-initialize here, so it
/// just announces itself.
fn start() callconv(.{ .wasm_mvp = .{} }) void {
    std.log.info("Ready to fill in severity numbers from severity_text", .{});
}

/// Host calls this with `(ptr, size)` referring to a buffer it just
/// wrote into our linear memory via `wasm4otel_alloc`. Decode, fill in
/// the severity numbers we can infer, re-encode, forward via
/// `push_logs`. The host calls `wasm4otel_free` as soon as we return —
/// do not retain the pointer.
fn processLogs(ptr: [*]const u8, size: usize) callconv(.{ .wasm_mvp = .{} }) i32 {
    var arena: std.heap.ArenaAllocator = .init(std.heap.wasm_allocator);
    defer arena.deinit();
    const alloc = arena.allocator();

    var reader: Io.Reader = .fixed(ptr[0..size]);
    var batch = LogsBatch.decode(&reader, alloc) catch |err| {
        std.log.err("decode failed: {t}", .{err});
        return 1;
    };

    const filled = fillSeverityNumbers(&batch);
    if (filled > 0) {
        std.log.debug("Filled in {d} severity number(s) from severity_text", .{filled});
    }

    var out: Io.Writer.Allocating = .init(alloc);
    defer out.deinit();
    batch.encode(&out.writer, alloc) catch |err| {
        std.log.err("encode failed: {t}", .{err});
        return 2;
    };

    return forward(out.written());
}

/// Give every record that has a `severity_text` we recognise but no
/// `severity_number` the number that text maps to. Records that
/// already carry a number are left untouched — the producer's explicit
/// value outranks anything we'd infer — and so are texts outside the
/// recognised set. Returns how many records changed.
fn fillSeverityNumbers(batch: *LogsBatch) usize {
    var filled: usize = 0;
    for (batch.resource_logs.items) |*resource_logs| {
        for (resource_logs.scope_logs.items) |*scope_logs| {
            for (scope_logs.log_records.items) |*record| {
                if (record.severity_number != .SEVERITY_NUMBER_UNSPECIFIED) continue;
                record.severity_number = parseSeverity(record.severity_text) orelse continue;
                filled += 1;
            }
        }
    }
    return filled;
}

/// Map a textual severity to an OTLP `SeverityNumber`. Case-insensitive.
/// Returns `null` for anything outside the recognised set.
fn parseSeverity(text: []const u8) ?SeverityNumber {
    const Entry = struct { []const u8, SeverityNumber };
    const table = [_]Entry{
        .{ "TRACE", .SEVERITY_NUMBER_TRACE },
        .{ "DEBUG", .SEVERITY_NUMBER_DEBUG },
        .{ "INFO", .SEVERITY_NUMBER_INFO },
        .{ "WARN", .SEVERITY_NUMBER_WARN },
        .{ "WARNING", .SEVERITY_NUMBER_WARN },
        .{ "ERROR", .SEVERITY_NUMBER_ERROR },
        .{ "FATAL", .SEVERITY_NUMBER_FATAL },
    };
    for (table) |entry| {
        if (std.ascii.eqlIgnoreCase(text, entry[0])) return entry[1];
    }
    return null;
}

/// Hand the re-encoded batch to the next consumer. Test builds run
/// under wasmtime, which supplies no `env` module to import from — the
/// same reason `std_options` swaps the logger above — so they park the
/// bytes in `test_sink` for the test to decode instead.
fn forward(bytes: []const u8) i32 {
    if (builtin.is_test) {
        test_sink.len = @min(bytes.len, test_sink.buffer.len);
        @memcpy(test_sink.buffer[0..test_sink.len], bytes[0..test_sink.len]);
        return 0;
    }
    return push_logs(bytes.ptr, bytes.len);
}

/// Stand-in for the next consumer, present only in test builds so the
/// real plugin carries none of it.
const test_sink = if (builtin.is_test) struct {
    var buffer: [4096]u8 = undefined;
    var len: usize = 0;
} else struct {};

/// One record, one scope, one resource, with the severity fields under
/// test. The arena is the caller's: `LogsData.deinit` frees every
/// `[]const u8` the message holds, which would mean freeing the string
/// literals passed in here, so these batches are never deinit'd
/// individually.
fn testBatch(alloc: std.mem.Allocator, number: SeverityNumber, text: []const u8) !LogsBatch {
    var batch: LogsBatch = .{};
    try batch.resource_logs.append(alloc, .{});
    const resource_logs = &batch.resource_logs.items[0];
    try resource_logs.scope_logs.append(alloc, .{});
    const scope_logs = &resource_logs.scope_logs.items[0];
    try scope_logs.log_records.append(alloc, .{
        .severity_number = number,
        .severity_text = text,
    });
    return batch;
}

/// The single record's severity, for assertions.
fn severityOf(batch: LogsBatch) SeverityNumber {
    return batch.resource_logs.items[0].scope_logs.items[0].log_records.items[0].severity_number;
}

test "parseSeverity recognises canonical names" {
    try std.testing.expectEqual(SeverityNumber.SEVERITY_NUMBER_INFO, parseSeverity("INFO").?);
    try std.testing.expectEqual(SeverityNumber.SEVERITY_NUMBER_WARN, parseSeverity("warn").?);
    try std.testing.expectEqual(SeverityNumber.SEVERITY_NUMBER_WARN, parseSeverity("Warning").?);
    try std.testing.expectEqual(SeverityNumber.SEVERITY_NUMBER_FATAL, parseSeverity("fatal").?);
}

test "parseSeverity rejects unknown text" {
    try std.testing.expectEqual(null, parseSeverity(""));
    try std.testing.expectEqual(null, parseSeverity("foobar"));
    try std.testing.expectEqual(null, parseSeverity("INFO2"));
}

test "fillSeverityNumbers infers the number from severity_text" {
    var arena: std.heap.ArenaAllocator = .init(std.testing.allocator);
    defer arena.deinit();

    var batch = try testBatch(arena.allocator(), .SEVERITY_NUMBER_UNSPECIFIED, "warning");
    try std.testing.expectEqual(1, fillSeverityNumbers(&batch));
    try std.testing.expectEqual(SeverityNumber.SEVERITY_NUMBER_WARN, severityOf(batch));
}

test "fillSeverityNumbers leaves an explicit number alone" {
    var arena: std.heap.ArenaAllocator = .init(std.testing.allocator);
    defer arena.deinit();

    var batch = try testBatch(arena.allocator(), .SEVERITY_NUMBER_ERROR, "debug");
    try std.testing.expectEqual(0, fillSeverityNumbers(&batch));
    try std.testing.expectEqual(SeverityNumber.SEVERITY_NUMBER_ERROR, severityOf(batch));
}

test "fillSeverityNumbers ignores text it cannot parse" {
    var arena: std.heap.ArenaAllocator = .init(std.testing.allocator);
    defer arena.deinit();

    var batch = try testBatch(arena.allocator(), .SEVERITY_NUMBER_UNSPECIFIED, "notalevel");
    try std.testing.expectEqual(0, fillSeverityNumbers(&batch));
    try std.testing.expectEqual(SeverityNumber.SEVERITY_NUMBER_UNSPECIFIED, severityOf(batch));
}

test "processLogs forwards a batch with the severity number filled in" {
    var arena: std.heap.ArenaAllocator = .init(std.testing.allocator);
    defer arena.deinit();
    const alloc = arena.allocator();

    const batch = try testBatch(alloc, .SEVERITY_NUMBER_UNSPECIFIED, "FATAL");
    var encoded: Io.Writer.Allocating = .init(alloc);
    try batch.encode(&encoded.writer, alloc);
    const payload = encoded.written();

    try std.testing.expectEqual(0, processLogs(payload.ptr, payload.len));

    var reader: Io.Reader = .fixed(test_sink.buffer[0..test_sink.len]);
    const forwarded = try LogsBatch.decode(&reader, alloc);
    try std.testing.expectEqual(SeverityNumber.SEVERITY_NUMBER_FATAL, severityOf(forwarded));
}
