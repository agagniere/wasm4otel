const std = @import("std");
const zon = @import("build.zig.zon");
const name = @tagName(zon.name);

const RunProtocStep = @import("protobuf").RunProtocStep;

pub fn build(b: *std.Build) !void {
    // Targets & optimization settings
    const native = b.resolveTargetQuery(.{});
    const freestanding = b.resolveTargetQuery(.{
        .cpu_arch = .wasm32,
        .os_tag = .freestanding,
        .cpu_model = .{ .explicit = &std.Target.wasm.cpu.mvp },
    });
    const wasip1 = b.resolveTargetQuery(.{
        .cpu_arch = .wasm32,
        .os_tag = .wasi,
        .os_version_min = .{ .semver = .{ .major = 0, .minor = 1, .patch = 0 } },
        .os_version_max = .{ .semver = .{ .major = 0, .minor = 1, .patch = 99 } },
        // https://github.com/wazero/wazero/blob/main/api/features.go#L30
        .cpu_model = .{ .explicit = &std.Target.wasm.cpu.lime1 },
        .cpu_features_add = std.Target.wasm.featureSet(&.{ .bulk_memory, .reference_types, .simd128 }),
    });
    const optimize = b.standardOptimizeOption(.{ .preferred_optimize_mode = .ReleaseSmall });

    // Dependencies
    const protobuf = b.dependency("protobuf", .{});
    const protobuf_module = protobuf.module("protobuf");
    const otelproto = b.dependency("otelproto", .{});

    // Build info
    const build_info = b.addOptions();
    build_info.addOption([]const u8, "version", zon.version);
    build_info.addOption([]const u8, "name", name);

    // Generate from protobuf
    const gen_proto = b.step("gen-proto", "Generate zig files from protocol buffer definitions");
    const run_protoc = RunProtocStep.create(protobuf.builder, native, .{
        // out directory for the generated zig files
        .destination_directory = b.path("src"),
        .source_files = &.{
            otelproto.path("opentelemetry/proto/logs/v1/logs.proto").getPath(b),
            otelproto.path("opentelemetry/proto/metrics/v1/metrics.proto").getPath(b),
            otelproto.path("opentelemetry/proto/trace/v1/trace.proto").getPath(b),
            // automatically imported:
            //otelproto.path("opentelemetry/proto/common/v1/common.proto").getPath(b),
            //otelproto.path("opentelemetry/proto/resource/v1/resource.proto").getPath(b),
        },
        .include_directories = &.{
            otelproto.path("").getPath(b),
        },
    });
    gen_proto.dependOn(&run_protoc.step);

    // Common modules
    const hostLog = b.addModule("hostlog", .{ .root_source_file = b.path("src/log.zig") });
    const otelData = b.addModule("otel_pipeline_data", .{ .root_source_file = b.path("src/pipeline.zig") });

    for (freestanding_sources) |source| {
        var mod = b.createModule(.{
            .root_source_file = b.path("freestanding").path(b, source.filename),
            .target = freestanding,
            .optimize = optimize,
            .imports = &.{
                .{ .name = "protobuf", .module = protobuf_module },
                .{ .name = "hostlog", .module = hostLog },
            },
        });
        mod.export_symbol_names = source.symbols;
        const exe = b.addExecutable(.{
            .name = source.name(),
            .root_module = mod,
        });
        b.installArtifact(exe);
        exe.step.dependOn(&run_protoc.step);
        exe.root_module.addOptions("build_info", build_info);
    }

    for (wasip1_sources) |source| {
        var mod = b.createModule(.{
            .root_source_file = b.path("wasip1").path(b, source.filename),
            .target = wasip1,
            .optimize = optimize,
            .imports = &.{
                .{ .name = "protobuf", .module = protobuf_module },
                .{ .name = "hostlog", .module = hostLog },
                .{ .name = "otel_pipeline_data", .module = otelData },
            },
        });
        mod.export_symbol_names = source.symbols;
        var exe = b.addExecutable(.{
            .name = source.name(),
            .root_module = mod,
        });
        exe.wasi_exec_model = .reactor;
        b.installArtifact(exe);
        exe.step.dependOn(&run_protoc.step);
        exe.root_module.addOptions("build_info", build_info);
    }
}

const OtelPlugin = struct {
    filename: []const u8,
    symbols: []const []const u8,

    pub fn name(self: OtelPlugin) []const u8 {
        return std.mem.cutSuffix(u8, self.filename, ".zig").?;
    }
};

const freestanding_sources: []const OtelPlugin = &.{
    .{ .filename = "helloworld.zig", .symbols = &.{ "start", "stop" } },
};

const wasip1_sources: []const OtelPlugin = &.{
    .{ .filename = "log_generator.zig", .symbols = &.{ "start", "stop" } },
};
