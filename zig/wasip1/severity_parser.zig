//! Tiny demonstration plugin: parses textual severity names
//! (`"INFO"`, `"warn"`, …) into OTLP `SeverityNumber` values.
//!
//! Its real purpose is to show how unit tests get wired up — see the
//! `test {}` blocks at the bottom and the `tests = true` flag in
//! `build.zig`. Run them with:
//!
//!     zig build test -fwasmtime
//!
//! `-fwasmtime` lets the build runner invoke wasmtime against the
//! compiled `wasm32-wasi` test binary.

const std = @import("std");
const builtin = @import("builtin");
const build_info = @import("build_info");
const host = @import("host");
const otelData = @import("otel_pipeline_data");

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

comptime {
    @export(&init, .{ .name = "_initialize" });
}

fn init() callconv(.{ .wasm_mvp = .{} }) void {
    std.log.info("Initializing {s}, part of {s} version {s}, built with Zig {s}", .{
        @src().file,
        build_info.name,
        build_info.version,
        builtin.zig_version_string,
    });
}

export fn start() void {
    const sample = "WARN";
    if (parseSeverity(sample)) |severity| {
        std.log.info("Parsed \"{s}\" -> {t}", .{ sample, severity });
    } else {
        std.log.warn("Could not parse \"{s}\"", .{sample});
    }
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
