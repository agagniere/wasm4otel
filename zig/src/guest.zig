//! This file contains functions that the guest plugins can
//! export, to allow the host to call them.
//!
//! Helpers for plugins acting as processors or exporters: the
//! `wasm4otel_alloc` and `wasm4otel_free` exports the host calls
//! around `wasm4otel_process_*` / `wasm4otel_export_*`. Plugins
//! re-export the two functions under their wire names via `@export`
//! at their own comptime block.

const std = @import("std");

/// Return type for the `wasm4otel_setup` guest export. `0 = success`,
/// `1 = generic failure`, `2 = invalid user-provided config`. Setup is
/// where the plugin reads `plugin_config` (see `host.getConfigAlloc`),
/// so it is the hook that carries the config code — the host runs it
/// inside the factory, and a non-zero return stops the collector from
/// finishing its boot. The non-exhaustive tail lets the host evolve
/// more specific codes later without breaking the ABI. The plugin
/// should `host_log` its own detail before returning anything
/// non-zero.
pub const SetupResult = enum(i32) {
    success = 0,
    failure = 1,
    invalid_config = 2,
    _,
};

/// Return type for the `wasm4otel_start` and `wasm4otel_receive`
/// guest exports. `0 = success`, `1 = generic failure`. Config has
/// already been validated by setup at this point, so the only question
/// left is whether work started — hence no `invalid_config`. Same
/// non-exhaustive tail and same `host_log` convention as
/// `SetupResult`.
pub const StartResult = enum(i32) {
    success = 0,
    failure = 1,
    _,
};

/// Reserve `size` bytes in linear memory and return the offset.
/// Returns 0 if the underlying allocator can't satisfy the request —
/// the host treats 0 as a non-trapping failure and does not call
/// `wasm4otel_free` on it.
pub fn alloc(size: u32) callconv(.{ .wasm_mvp = .{} }) u32 {
    if (size == 0) return 0;
    const slice = std.heap.wasm_allocator.alloc(u8, size) catch return 0;
    return @intFromPtr(slice.ptr);
}

/// Release a region previously returned by `wasm4otel_alloc`. `size`
/// is the same size that was passed to alloc — the host carries it for
/// us so our allocator doesn't need a per-block header.
pub fn free(ptr: u32, size: u32) callconv(.{ .wasm_mvp = .{} }) void {
    if (ptr == 0 or size == 0) return;
    const slice = @as([*]u8, @ptrFromInt(ptr))[0..size];
    std.heap.wasm_allocator.free(slice);
}
