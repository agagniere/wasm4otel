const std = @import("std");

/// Used to issue a log to be forwared to zap
extern fn host_log(level: i32, ptr: [*]const u8, size: u32) void;

/// https://pkg.go.dev/go.uber.org/zap@v1.27.1/zapcore#Level
const LogLevel = enum(i8) {
    debug = -1,
    info,
    warn,
    err,
    dpanic,
    panic,
    fatal,

    fn fromStd(level: std.log.Level) LogLevel {
        return switch (level) {
            .debug => .debug,
            .info => .info,
            .warn => .warn,
            .err => .err,
        };
    }
};

/// Forward this string to zap
pub fn hostLog(level: LogLevel, string: []const u8) void {
    host_log(@intFromEnum(level), string.ptr, string.len);
}

/// Format then forward this log to zap
pub fn hostLogFormat(level: LogLevel, format: []const u8, args: anytype) void {
    var buffer: [256]u8 = undefined;
    var writer: std.Io.Writer = .fixed(&buffer);
    writer.print(format, args) catch unreachable;
    hostLog(level, writer.buffered());
}

/// To be used as callback of the standard log,
/// to forward std.log calls to zap
pub fn logFn(
    comptime level: std.log.Level,
    comptime scope: @EnumLiteral(),
    comptime format: []const u8,
    args: anytype,
) void {
    var buffer: [256]u8 = undefined;
    var writer: std.Io.Writer = .fixed(&buffer);
    if (scope != .default) {
        writer.print("[{t}] ", .{scope}) catch unreachable;
    }
    writer.print(format, args) catch unreachable;
    hostLog(.fromStd(level), writer.buffered());
}
