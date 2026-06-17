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
        // Pin the WASI min to 0.1.0 so `--summary all` tags the target
        // as `wasm32-wasi.0.1`, making preview1 vs preview2 obvious.
        // Functionally a no-op on Zig 0.16 — stdlib doesn't branch on
        // the WASI version range — but cheap insurance once it does.
        .os_version_min = .{ .semver = .{ .major = 0, .minor = 1, .patch = 0 } },
        // https://github.com/wazero/wazero/blob/main/api/features.go#L30
        .cpu_model = .{ .explicit = &std.Target.wasm.cpu.lime1 },
        .cpu_features_add = std.Target.wasm.featureSet(&.{ .bulk_memory, .reference_types, .simd128 }),
    });
    const optimize = b.standardOptimizeOption(.{ .preferred_optimize_mode = .ReleaseSmall });

    // Dependencies
    const protobuf = b.dependency("protobuf", .{ .optimize = optimize });
    const protobuf_module = protobuf.module("protobuf");
    const otelproto = b.dependency("otelproto", .{});

    // Build info
    const build_info = b.addOptions();
    build_info.addOption([]const u8, "version", zon.version);
    build_info.addOption([]const u8, "name", name);
    const build_info_mod = build_info.createModule();

    // Generate from protobuf
    const gen_proto = b.step("gen-proto", "Generate zig files from protocol buffer definitions");
    const run_protoc = RunProtocStep.create(protobuf.builder, native, .{
        // out directory for the generated zig files
        .destination_directory = b.path("src"),
        .source_files = &.{
            otelproto.path("opentelemetry/proto/logs/v1/logs.proto"),
            otelproto.path("opentelemetry/proto/metrics/v1/metrics.proto"),
            otelproto.path("opentelemetry/proto/trace/v1/trace.proto"),
            // automatically imported:
            //otelproto.path("opentelemetry/proto/common/v1/common.proto"),
            //otelproto.path("opentelemetry/proto/resource/v1/resource.proto"),
        },
        .include_directories = &.{
            otelproto.path(""),
        },
    });
    gen_proto.dependOn(&run_protoc.step);

    // Common modules
    const host = b.addModule("host", .{ .root_source_file = b.path("src/host.zig") });
    const guest = b.addModule("guest", .{ .root_source_file = b.path("src/guest.zig") });
    const otelData = b.addModule("otel_pipeline_data", .{
        .root_source_file = b.path("src/pipeline.zig"),
        .imports = &.{.{ .name = "protobuf", .module = protobuf_module }},
    });

    const plugin_sets = [_]PluginSet{
        .{ .folder = "freestanding", .target = freestanding, .exec_model = null, .sources = freestanding_sources },
        .{ .folder = "wasip1", .target = wasip1, .exec_model = .reactor, .sources = wasip1_sources },
    };

    const test_step = b.step("test", "Run plugin tests under wasmtime (requires -fwasmtime)");

    for (plugin_sets) |set| {
        for (set.sources) |source| {
            var mod = b.createModule(.{
                .root_source_file = b.path(set.folder).path(b, source.filename),
                .target = set.target,
                .optimize = optimize,
                .imports = &.{
                    .{ .name = "host", .module = host },
                    .{ .name = "guest", .module = guest },
                    .{ .name = "otel_pipeline_data", .module = otelData },
                    .{ .name = "build_info", .module = build_info_mod },
                },
            });
            mod.export_symbol_names = source.symbols;
            var exe = b.addExecutable(.{
                .name = source.name(),
                .root_module = mod,
            });
            exe.wasi_exec_model = set.exec_model;
            b.installArtifact(exe);
            exe.step.dependOn(&run_protoc.step);

            if (source.tests) {
                const test_exe = b.addTest(.{
                    .name = b.fmt("{s}-test", .{source.name()}),
                    .root_module = mod,
                });
                test_exe.step.dependOn(&run_protoc.step);
                test_step.dependOn(&b.addRunArtifact(test_exe).step);
            }
        }
    }
}

const PluginSet = struct {
    folder: []const u8,
    target: std.Build.ResolvedTarget,
    /// `null` for non-WASI targets
    exec_model: ?std.builtin.WasiExecModel,
    sources: []const OtelPlugin,
};

const OtelPlugin = struct {
    filename: []const u8,
    symbols: []const []const u8,
    tests: bool = false,

    pub fn name(self: OtelPlugin) []const u8 {
        return std.mem.cutSuffix(u8, self.filename, ".zig").?;
    }
};

const freestanding_sources: []const OtelPlugin = &.{
    .{ .filename = "helloworld.zig", .symbols = &.{ "start", "stop" } },
    .{ .filename = "one_log.zig", .symbols = &.{"start"} },
};

const wasip1_sources: []const OtelPlugin = &.{
    .{ .filename = "log_generator.zig", .symbols = &.{ "start", "stop" } },
    .{ .filename = "severity_parser.zig", .symbols = &.{"start"}, .tests = true },
};
