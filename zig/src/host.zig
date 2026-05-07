//! Wrappers over host-provided primitives that aren't OTLP plumbing.

const std = @import("std");

/// Host import. Sleeps for at most `ms` milliseconds. Returns 0 when
/// the duration elapsed, non-zero when the host interrupted the sleep
/// (e.g. component shutdown).
extern fn interruptible_sleep_ms(ms: u32) u32;

pub const SleepError = error{Interrupted};

/// Sleep for `milliseconds`.
/// Returns `error.Interrupted` if the host woke the sleep early.
pub fn interruptibleMilliSleep(milliseconds: u32) SleepError!void {
    return switch (interruptible_sleep_ms(milliseconds)) {
        0 => {},
        else => error.Interrupted,
    };
}

/// Sleep for `seconds`. Convenience wrapper around `interruptibleMilliSleep`.
///
/// `u16` caps the input at ~18 hours
pub fn interruptibleSleep(seconds: u16) SleepError!void {
    return interruptibleMilliSleep(@as(u32, seconds) * std.time.ms_per_s);
}
