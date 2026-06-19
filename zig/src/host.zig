//! Wrappers over host-provided primitives that aren't OTLP plumbing.
//!
//! To clarify any possible confusion: This file contains functions
//! to be used by guest plugins, in order to call functions made available by the host

const std = @import("std");
const log = @import("log.zig");

/// Sleeps for at most `ms` milliseconds. Returns 0 when the duration
/// elapsed, non-zero when the host interrupted the sleep (e.g.
/// component shutdown).
extern fn interruptible_sleep_ms(ms: u32) u32;

/// Writes the YAML `plugin_config` map at `ptr` as a JSON document,
/// but only when `size` is at least the document's true byte length.
/// Returns the true length in either case (0 if no config was set),
/// so callers can probe with `(null, 0)` and re-call with a buffer
/// the right size.
extern fn get_config(ptr: ?[*]u8, size: u32) u32;

pub const hostLog = log.hostLog;
pub const hostLogFormat = log.hostLogFormat;
pub const logFn = log.logFn;

pub const SleepError = error{Interrupted};

/// Sleep for at most `duration`. Returns `error.Interrupted` if the
/// host woke the sleep early (typically component shutdown).
///
/// Build with `.fromSeconds(...)`, `.fromMilliseconds(...)`, etc.
/// Sub-millisecond precision is rounded down — the wire format is ms.
/// Durations are clamped to `[0, maxInt(u31)]` ms, so negative
/// durations sleep for zero and anything beyond ~24 days saturates.
pub fn interruptibleSleep(duration: std.Io.Duration) SleepError!void {
    const ms_i96 = @divTrunc(duration.nanoseconds, std.time.ns_per_ms);
    const ms: u32 = @intCast(std.math.clamp(ms_i96, 0, std.math.maxInt(u31)));
    return switch (interruptible_sleep_ms(ms)) {
        0 => {},
        else => error.Interrupted,
    };
}

/// Fetch the YAML `plugin_config` as a JSON document. Returns `null`
/// when no `plugin_config` was set; otherwise allocates and returns
/// the bytes — caller frees with `allocator.free`. Parse with
/// `std.json.parseFromSlice`.
pub fn getConfigAlloc(allocator: std.mem.Allocator) std.mem.Allocator.Error!?[]u8 {
    const size = get_config(null, 0);
    if (size == 0) return null;
    const buf = try allocator.alloc(u8, size);
    errdefer allocator.free(buf);
    std.debug.assert(get_config(buf.ptr, @intCast(buf.len)) == size);
    return buf;
}
