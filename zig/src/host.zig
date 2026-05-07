//! Wrappers over host-provided primitives that aren't OTLP plumbing.

const std = @import("std");

/// Host import. Sleeps for at most `ms` milliseconds. Returns 0 when
/// the duration elapsed, non-zero when the host interrupted the sleep
/// (e.g. component shutdown).
extern fn interruptible_sleep_ms(ms: u32) u32;

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
