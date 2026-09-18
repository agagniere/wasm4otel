package wasm4otel

import (
	std_context "context"
	std_errors "errors"
	std_fmt "fmt"
	std_io "io"
	std_json "encoding/json"
	std_os "os"
	std_rand "crypto/rand"
	std_sync "sync"
	std_time "time"

	uber_zap "go.uber.org/zap"
	uber_zapcore "go.uber.org/zap/zapcore"

	otel_component "go.opentelemetry.io/collector/component"
	otel_consumer "go.opentelemetry.io/collector/consumer"
	otel_logs "go.opentelemetry.io/collector/pdata/plog"
	otel_metrics "go.opentelemetry.io/collector/pdata/pmetric"
	otel_traces "go.opentelemetry.io/collector/pdata/ptrace"

	"github.com/tetratelabs/wazero"
	wazero_api "github.com/tetratelabs/wazero/api"
	wazero_sys "github.com/tetratelabs/wazero/sys"
	"github.com/tetratelabs/wazero/imports/wasi_snapshot_preview1"
)

// ComponentMode selects the lifecycle a Component runs under. It is
// supplied by the role-specific factory as a NewComponent argument.
type ComponentMode uint8

const (
	// ModeReceiver: wasm4otel_receive drives a long-running loop on its
	// own goroutine; Shutdown cancels the context and joins it.
	ModeReceiver ComponentMode = iota
	// ModeProcessor: pipeline goroutines drive the guest via
	// ConsumeLogs/Metrics/Traces, which call wasm4otel_process_<signal>.
	ModeProcessor
	// ModeExporter: same lifecycle as ModeProcessor but the guest is
	// terminal — the host calls wasm4otel_export_<signal> instead, and
	// no downstream consumer is wired, so push_logs from the guest goes
	// nowhere.
	ModeExporter
)

func (self ComponentMode) String() string {
	switch self {
	case ModeReceiver:
		return "receiver"
	case ModeProcessor:
		return "processor"
	case ModeExporter:
		return "exporter"
	default:
		return "unknown"
	}
}

// Return codes from the guest's `wasm4otel_setup` export. 0 = ok;
// 1 = generic failure; 2 = invalid user-provided config. Setup is
// where YAML mistakes get rejected, so it is the hook that carries the
// config code.
const (
	SetupSuccess       uint32 = 0
	SetupFailure       uint32 = 1
	SetupInvalidConfig uint32 = 2
)

// Return codes from the guest's `wasm4otel_start` and
// `wasm4otel_receive` exports. 0 = ok; 1 = generic failure. Config has
// already been validated by setup at this point, so the only question
// left is whether work started. The host distinguishes known codes in
// the surfaced error message, falling back to generic failure for
// forward-added codes. Guests are expected to host_log their own
// details before returning non-zero.
const (
	StartSuccess uint32 = 0
	StartFailure uint32 = 1
)

type Component struct {
	mode ComponentMode

	logger      *uber_zap.SugaredLogger
	guestLogger *uber_zap.SugaredLogger
	context     std_context.Context
	cancel      std_context.CancelFunc
	config      Config
	runtime     wazero.Runtime
	instance    wazero_api.Module

	// pluginConfigJSON is the YAML's plugin_config map marshalled to
	// JSON once at NewComponent time; the guest pulls it via the
	// get_config host import.
	pluginConfigJSON []byte

	setup    wazero_api.Function
	start    wazero_api.Function
	receive  wazero_api.Function
	shutdown wazero_api.Function

	// Batch exports are role-typed as well as signal-typed, so the
	// export table alone says which role the plugin was written for.
	// Only the pair matching this component's mode is ever called.
	processLogs    wazero_api.Function
	processMetrics wazero_api.Function
	processTraces  wazero_api.Function
	exportLogs     wazero_api.Function
	exportMetrics  wazero_api.Function
	exportTraces   wazero_api.Function

	alloc wazero_api.Function
	free  wazero_api.Function

	NextConsumerLogs    otel_consumer.Logs
	NextConsumerMetrics otel_consumer.Metrics
	NextConsumerTraces  otel_consumer.Traces

	receiveWg std_sync.WaitGroup

	// callMu serializes every Call() into the wasm instance. wazero's
	// Module.Call is single-occupant; without this two pipeline
	// goroutines could race on a shared linear memory.
	callMu std_sync.Mutex
	// broken latches once any guest Call() traps. wazero leaves the
	// instance in an undefined state after a trap, so we refuse further
	// entries instead of compounding the corruption.
	broken bool
}

// Load is the one-shot factory entrypoint: it builds a Component,
// registers the host imports, loads the plugin from disk, verifies the
// exports the mode requires regardless of pipeline signal, then runs
// the guest's wasm4otel_setup hook. Because it all happens inside the
// factory's createX, a plugin that rejects its plugin_config stops the
// collector from finishing its boot rather than failing at the first
// batch. Signal-specific validation (e.g. ValidateLogsExport) is the
// caller's responsibility and runs once this returns.
func Load(
	anyconfig otel_component.Config,
	logger *uber_zap.Logger,
	mode ComponentMode,
) (*Component, error) {
	component, err := NewComponent(anyconfig, logger, mode)
	if err != nil {
		return nil, err
	}
	if err = component.ExposeFunctionsToGuest(); err != nil {
		return nil, err
	}
	if err = component.LoadPlugin(); err != nil {
		return nil, err
	}
	if err = component.validateModeExports(); err != nil {
		return nil, err
	}
	if err = component.runSetup(); err != nil {
		return nil, err
	}
	return component, nil
}

// validateModeExports checks the exports a role needs whatever signal
// it was wired for: a receiver needs a loop to run, a processor or
// exporter needs to be able to take the batch the host hands it.
// Checks are additive — a plugin exporting more than this role uses is
// fine, it simply plays one role per Component instance.
func (self *Component) validateModeExports() error {
	switch self.mode {
	case ModeReceiver:
		if self.receive == nil {
			return std_fmt.Errorf("wasm4otel %s: plugin must export wasm4otel_receive", self.mode)
		}
	case ModeProcessor, ModeExporter:
		if !self.HasAllocFree() {
			return std_fmt.Errorf("wasm4otel %s: plugin must export wasm4otel_alloc and wasm4otel_free", self.mode)
		}
	}
	return nil
}

// runSetup invokes the guest's wasm4otel_setup export, its chance to
// read plugin_config through get_config and refuse it. A plugin that
// doesn't export it is taken to have nothing to validate.
func (self *Component) runSetup() error {
	if self.setup == nil {
		return nil
	}
	results, err := self.invoke(self.context, self.setup)
	if err != nil {
		self.guestLogger.Warnw("setup trapped", "error", err)
		return err
	}
	if err := self.setupResultError(results); err != nil {
		self.guestLogger.Errorw("setup reported failure", "error", err)
		return err
	}
	return nil
}

// setupResultError maps the guest's wasm4otel_setup return code to an
// error. invalid_config gets its own wording because it is the code an
// operator can act on — their YAML is what's wrong. Empty results are
// treated as success, like the other lifecycle hooks.
func (self *Component) setupResultError(results []uint64) error {
	if len(results) == 0 {
		return nil
	}
	switch rc := uint32(results[0]); rc {
	case SetupSuccess:
		return nil
	case SetupInvalidConfig:
		return std_fmt.Errorf("wasm4otel %s: plugin rejected plugin_config as invalid (see plugin logs)", self.mode)
	default:
		return std_fmt.Errorf("wasm4otel %s: plugin setup failed with code %d (see plugin logs)", self.mode, rc)
	}
}

func NewComponent(
	anyconfig otel_component.Config,
	logger *uber_zap.Logger,
	mode ComponentMode,
) (*Component, error) {
	hostLogger := logger.Sugar()
	config := anyconfig.(Config)
	if err := config.Validate(); err != nil {
		return nil, err
	}
	var pluginConfigJSON []byte
	if config.PluginConfig != nil {
		var err error
		pluginConfigJSON, err = std_json.Marshal(config.PluginConfig)
		if err != nil {
			return nil, std_fmt.Errorf("wasm4otel %s: marshal plugin_config: %w", mode, err)
		}
	}
	hostLogger.Infow("Loading WebAssembly plugin", "path", config.Path)
	// Detach from the framework's create-phase ctx — the component
	// owns its own cancellation, ended only by Shutdown via `cancel`.
	context, cancel := std_context.WithCancel(std_context.Background())
	runtime := newRuntime(context)
	return &Component{
		mode:             mode,
		logger:           hostLogger,
		context:          context,
		cancel:           cancel,
		config:           config,
		runtime:          runtime,
		pluginConfigJSON: pluginConfigJSON,
	}, nil
}

func newRuntime(context std_context.Context) wazero.Runtime {
	runtime := wazero.NewRuntimeWithConfig(context, wazero.NewRuntimeConfigInterpreter())
	wasi_snapshot_preview1.MustInstantiate(context, runtime)
	return runtime
}

// Provide a callback accessible from the guest to log
// and functions to push logs/metrics/traces to the next consumer
func (self *Component) ExposeFunctionsToGuest() error {
	self.guestLogger = self.logger.With("plugin", self.config.Path)
	_, err := self.runtime.NewHostModuleBuilder("env").
		NewFunctionBuilder().WithFunc(self.logToZap).Export("host_log").
		NewFunctionBuilder().WithFunc(self.outboundLogs).Export("push_logs").
		NewFunctionBuilder().WithFunc(self.outboundMetrics).Export("push_metrics").
		NewFunctionBuilder().WithFunc(self.outboundTraces).Export("push_traces").
		NewFunctionBuilder().WithFunc(self.interruptibleSleepMs).Export("interruptible_sleep_ms").
		NewFunctionBuilder().WithFunc(self.getConfig).Export("get_config").
		Instantiate(self.context)
	return err
}

// getConfig hands the JSON-encoded plugin_config to the guest in one
// call. Returns the true size in bytes regardless of whether anything
// was written:
//
//   - returns 0 if there is no plugin_config to fetch;
//   - if `size >= true_size`, writes the bytes at `ptr` and returns
//     true_size (= bytes written);
//   - if `size < true_size`, writes nothing and returns true_size so
//     the guest knows to re-allocate and call again.
//
// Probing with `(0, 0)` is supported: it returns the size without
// touching guest memory. JSON can't be parsed partially, so we refuse
// to write a truncated payload instead of trying to be helpful.
func (self *Component) getConfig(_ std_context.Context, module wazero_api.Module, ptr uint32, size uint32) uint32 {
	total := uint32(len(self.pluginConfigJSON))
	if total == 0 {
		return 0
	}
	if size < total {
		return total
	}
	if !module.Memory().Write(ptr, self.pluginConfigJSON) {
		self.guestLogger.Errorw("get_config: memory write failed", "ptr", ptr, "size", total)
		return 0
	}
	return total
}

func (self *Component) outboundLogs(
	_ std_context.Context,
	module wazero_api.Module,
	offset uint32,
	size uint32,
) uint32 {
	buffer, ok := module.Memory().Read(offset, size)
	if !ok {
		self.guestLogger.Errorf("Unable to read (%d, %d) from memory", offset, size)
		return 1
	}
	deserializer := otel_logs.ProtoUnmarshaler{}
	logs, err := deserializer.UnmarshalLogs(buffer)
	if err != nil {
		self.guestLogger.Errorw("Unable to deserialize logs", "size", size, "error", err)
		return 2
	}
	if self.NextConsumerLogs == nil {
		self.guestLogger.Error("Plugin is pushing logs to a dead-end")
		return 3
	}
	if err := self.NextConsumerLogs.ConsumeLogs(self.context, logs); err != nil {
		self.guestLogger.Errorw("Downstream consumer rejected batch", "size", size, "error", err)
		return 4
	}
	self.guestLogger.Infow("OK", "bytes", size)
	return 0
}

func (self *Component) outboundMetrics(
	_ std_context.Context,
	module wazero_api.Module,
	offset uint32,
	size uint32,
) uint32 {
	buffer, ok := module.Memory().Read(offset, size)
	if !ok {
		self.guestLogger.Errorf("Unable to read (%d, %d) from memory", offset, size)
		return 1
	}
	deserializer := otel_metrics.ProtoUnmarshaler{}
	metrics, err := deserializer.UnmarshalMetrics(buffer)
	if err != nil {
		self.guestLogger.Errorw("Unable to deserialize metrics", "size", size, "error", err)
		return 2
	}
	if self.NextConsumerMetrics == nil {
		self.guestLogger.Error("Plugin is pushing metrics to a dead-end")
		return 3
	}
	if err := self.NextConsumerMetrics.ConsumeMetrics(self.context, metrics); err != nil {
		self.guestLogger.Errorw("Downstream consumer rejected batch", "size", size, "error", err)
		return 4
	}
	self.guestLogger.Infow("OK", "bytes", size)
	return 0
}

func (self *Component) outboundTraces(
	_ std_context.Context,
	module wazero_api.Module,
	offset uint32,
	size uint32,
) uint32 {
	buffer, ok := module.Memory().Read(offset, size)
	if !ok {
		self.guestLogger.Errorf("Unable to read (%d, %d) from memory", offset, size)
		return 1
	}
	deserializer := otel_traces.ProtoUnmarshaler{}
	traces, err := deserializer.UnmarshalTraces(buffer)
	if err != nil {
		self.guestLogger.Errorw("Unable to deserialize traces", "size", size, "error", err)
		return 2
	}
	if self.NextConsumerTraces == nil {
		self.guestLogger.Error("Plugin is pushing traces to a dead-end")
		return 3
	}
	if err := self.NextConsumerTraces.ConsumeTraces(self.context, traces); err != nil {
		self.guestLogger.Errorw("Downstream consumer rejected batch", "size", size, "error", err)
		return 4
	}
	self.guestLogger.Infow("OK", "bytes", size)
	return 0
}

func (self *Component) Start(_ std_context.Context, _ otel_component.Host) error {
	// wasm4otel_start means the same thing in every mode: short-lived
	// init that didn't fit in setup, e.g. opening files now that the
	// config is known to be good. It must return promptly so the
	// pipeline can begin work.
	if self.start != nil {
		results, err := self.invoke(self.context, self.start)
		if err != nil {
			self.guestLogger.Warnw("start trapped", "error", err)
			return err
		}
		if err := self.startResultError(results, "wasm4otel_start"); err != nil {
			self.guestLogger.Errorw("start reported failure", "error", err)
			return err
		}
	}
	// Only a receiver-mode Component runs the receive loop, even though
	// a multi-role plugin may well export it: each YAML section the
	// plugin is wired into gets its own Component instance playing
	// exactly one role, and a processor has no business also generating
	// telemetry. validateModeExports guarantees the export is there.
	if self.mode == ModeReceiver {
		self.receiveWg.Add(1)
		go self.receiveLoop()
	}
	return nil
}

// receiveLoop runs the guest's wasm4otel_receive export to completion
// on its own goroutine. The loop owns the instance until Shutdown
// cancels the context, so it runs without callMu; Shutdown waits for it
// to drain before calling wasm4otel_shutdown.
func (self *Component) receiveLoop() {
	defer self.receiveWg.Done()
	results, err := self.receive.Call(self.context)
	if err != nil {
		// receive runs outside invoke() (callMu would be useless for a
		// lifetime-long loop), so latch broken here to make the shutdown
		// invoke() in Shutdown see the poisoned instance.
		self.callMu.Lock()
		self.broken = true
		self.callMu.Unlock()
		self.guestLogger.Warnw("receive trapped", "error", err)
		self.cancel()
		return
	}
	if err := self.startResultError(results, "wasm4otel_receive"); err != nil {
		self.guestLogger.Errorw("receive reported failure", "error", err)
		self.cancel()
	}
}

// startResultError maps a StartResult return code (if any) to an error.
// Shared by wasm4otel_start and wasm4otel_receive, which allocate their
// codes from the same enum. Empty results — a guest declaring the export
// as `() -> ()` — are treated as success for forward compatibility; the
// convention is `() -> i32` so the plugin can signal failure.
func (self *Component) startResultError(results []uint64, export string) error {
	if len(results) == 0 {
		return nil
	}
	if rc := uint32(results[0]); rc != StartSuccess {
		return std_fmt.Errorf("wasm4otel %s: plugin %s failed with code %d (see plugin logs)", self.mode, export, rc)
	}
	return nil
}

func (self *Component) Shutdown(context std_context.Context) error {
	// Cancel the plugin's context first so any blocking host import
	// (interruptible_sleep_ms, push_logs) returns to the guest with a
	// cancellation signal; the receive loop unwinds on its own, then we
	// wait. Processor/exporter modes have no goroutine to wait on, so
	// the Wait is a no-op there.
	self.cancel()
	self.receiveWg.Wait()
	if self.shutdown != nil {
		if _, err := self.invoke(context, self.shutdown); err != nil {
			self.guestLogger.Warnw("shutdown returned with error", "error", err)
		}
	}
	return nil
}

// invoke takes callMu, refuses entry if the instance is poisoned, and
// latches `broken` if the call traps. Every guest entry except the
// receiver-mode start loop goes through here.
func (self *Component) invoke(ctx std_context.Context, fn wazero_api.Function, args ...uint64) ([]uint64, error) {
	self.callMu.Lock()
	defer self.callMu.Unlock()
	if self.broken {
		return nil, std_errors.New("wasm4otel: component is poisoned (prior trap)")
	}
	results, err := fn.Call(ctx, args...)
	if err != nil {
		self.broken = true
		return nil, err
	}
	return results, nil
}

// interruptibleSleepMs blocks for `ms` milliseconds, returning early
// when the component's context is cancelled (i.e. Shutdown ran).
// Returns 0 on full elapse, 1 on early wake-up.
func (self *Component) interruptibleSleepMs(_ std_context.Context, ms uint32) uint32 {
	select {
	case <-std_time.After(std_time.Duration(ms) * std_time.Millisecond):
		return 0
	case <-self.context.Done():
		return 1
	}
}

func (self *Component) logToZap(_ std_context.Context, module wazero_api.Module, level int32, offset uint32, size uint32) {
	buffer, ok := module.Memory().Read(offset, size)
	if !ok {
		self.guestLogger.Errorf("Unable to read (%d, %d) from memory", offset, size)
		return
	}
	self.guestLogger.Log(uber_zapcore.Level(level), string(buffer))
}

func (self *Component) LoadPlugin() error {
	plugin, err := std_os.Open(self.config.Path)
	if err != nil {
		self.logger.Errorw("Unable to open file",
			"path", self.config.Path,
			"error", err)
		return err
	}
	defer plugin.Close()

	bytes, err := std_io.ReadAll(plugin)
	if err != nil {
		self.logger.Errorw("Unable to read file contents",
			"path", self.config.Path,
			"error", err)
		return err
	}

	// Opt out of wazero's deterministic defaults: without these, a
	// guest sees a frozen 2022-01-01 wall clock and a fixed random
	// stream. Telemetry needs real timestamps, and trace/span IDs have
	// to be unpredictable and unique across restarts — a seeded source
	// would hand every collector instance the same IDs.
	// WithSysNanosleep is deliberately left out: plugins pace
	// themselves through the interruptible_sleep_ms host import, which
	// Shutdown can cancel, rather than a WASI sleep we have no handle
	// on.
	config := wazero.NewModuleConfig().
		WithStartFunctions("_start", "_initialize").
		WithSysWalltime().
		WithSysNanotime().
		WithRandSource(std_rand.Reader)
	//WithStdout(std_os.Stdout).
	//WithStderr(std_os.Stderr)
	//WithArgs("toto", "foo")

	instance, err := self.runtime.InstantiateWithConfig(self.context, bytes, config)
	if err != nil {
		if exitErr, ok := err.(*wazero_sys.ExitError); ok && exitErr.ExitCode() != 0 {
			std_fmt.Fprintf(std_os.Stderr, "exit_code: %d\n", exitErr.ExitCode())
		} else if !ok {
			self.logger.Panicln(err)
		}
		return err
	}

	// Every host-called export carries the wasm4otel_ prefix, so the
	// module's export table is self-evidently the ABI surface. It also
	// keeps `shutdown` from colliding with the POSIX socket call that
	// wasi-libc defines.
	self.instance = instance
	self.setup = instance.ExportedFunction("wasm4otel_setup")
	self.start = instance.ExportedFunction("wasm4otel_start")
	self.receive = instance.ExportedFunction("wasm4otel_receive")
	self.shutdown = instance.ExportedFunction("wasm4otel_shutdown")

	self.processLogs = instance.ExportedFunction("wasm4otel_process_logs")
	self.processMetrics = instance.ExportedFunction("wasm4otel_process_metrics")
	self.processTraces = instance.ExportedFunction("wasm4otel_process_traces")
	self.exportLogs = instance.ExportedFunction("wasm4otel_export_logs")
	self.exportMetrics = instance.ExportedFunction("wasm4otel_export_metrics")
	self.exportTraces = instance.ExportedFunction("wasm4otel_export_traces")

	// The allocator pair was prefixed before the rest of the ABI was,
	// because Rust + wasm32-wasi links wasi-libc, which already defines
	// `free`; an unprefixed export collides at link time.
	self.alloc = instance.ExportedFunction("wasm4otel_alloc")
	self.free = instance.ExportedFunction("wasm4otel_free")
	return nil
}

// HasAllocFree reports whether the guest exports the wasm4otel_alloc /
// wasm4otel_free pair. They are required by every processor/exporter
// signal — a guest missing them is genuinely incomplete, not just
// silent on one signal.
func (self *Component) HasAllocFree() bool {
	return self.alloc != nil && self.free != nil
}

// logsExport returns the guest export that serves the logs signal in
// the role this component plays, along with its ABI name for
// diagnostics. Only meaningful for ModeProcessor / ModeExporter — a
// receiver is never handed a batch, so neither the receiver factory nor
// the pipeline reaches this.
func (self *Component) logsExport() (wazero_api.Function, string) {
	if self.mode == ModeExporter {
		return self.exportLogs, "wasm4otel_export_logs"
	}
	return self.processLogs, "wasm4otel_process_logs"
}

// metricsExport: same as logsExport for the metrics signal.
func (self *Component) metricsExport() (wazero_api.Function, string) {
	if self.mode == ModeExporter {
		return self.exportMetrics, "wasm4otel_export_metrics"
	}
	return self.processMetrics, "wasm4otel_process_metrics"
}

// tracesExport: same as logsExport for the traces signal.
func (self *Component) tracesExport() (wazero_api.Function, string) {
	if self.mode == ModeExporter {
		return self.exportTraces, "wasm4otel_export_traces"
	}
	return self.processTraces, "wasm4otel_process_traces"
}

// ValidateLogsExport reports whether the guest can serve the logs
// signal in the role this component was created for. Used by the
// processor/exporter factories' createLogs hooks; receivers don't
// consume so they don't call this. The error names the export missing
// for *that* role: the operator either fixes the YAML section the
// plugin sits in, or rebuilds the plugin with the export it needs.
func (self *Component) ValidateLogsExport() error {
	fn, name := self.logsExport()
	return self.validateSignalExport(fn, name, "logs")
}

// ValidateMetricsExport reports whether the guest can serve the
// metrics signal in the role this component was created for.
func (self *Component) ValidateMetricsExport() error {
	fn, name := self.metricsExport()
	return self.validateSignalExport(fn, name, "metrics")
}

// ValidateTracesExport reports whether the guest can serve the traces
// signal in the role this component was created for.
func (self *Component) ValidateTracesExport() error {
	fn, name := self.tracesExport()
	return self.validateSignalExport(fn, name, "traces")
}

// validateSignalExport turns a missing batch export into the error the
// operator sees at boot: `name` is the export their plugin lacks for
// the role it was wired as, `signal` the pipeline section it sits in.
func (self *Component) validateSignalExport(fn wazero_api.Function, name string, signal string) error {
	if fn == nil {
		return std_fmt.Errorf("wasm4otel %s: plugin does not export %s; it does not support the %s signal in this role", self.mode, name, signal)
	}
	return nil
}

// Capabilities satisfies consumer.Logs/Metrics/Traces. We always
// re-marshal the incoming pdata to bytes before crossing into the
// guest, so we never mutate the caller's view.
func (self *Component) Capabilities() otel_consumer.Capabilities {
	return otel_consumer.Capabilities{MutatesData: false}
}

// ConsumeLogs marshals the batch to OTLP bytes and hands them to the
// guest via the alloc → write → batch export → free sequence, all under
// callMu. Which export that is depends on the mode: a processor gets
// wasm4otel_process_logs and forwards its transformed batch through the
// push_logs host import — that path uses NextConsumerLogs, not the
// return path — while an exporter gets wasm4otel_export_logs and is
// terminal.
func (self *Component) ConsumeLogs(ctx std_context.Context, logs otel_logs.Logs) error {
	payload, err := (&otel_logs.ProtoMarshaler{}).MarshalLogs(logs)
	if err != nil {
		return std_fmt.Errorf("marshal logs: %w", err)
	}
	fn, export := self.logsExport()
	return self.deliver(ctx, fn, payload, export)
}

// ConsumeMetrics: same shape as ConsumeLogs for the metrics signal.
func (self *Component) ConsumeMetrics(ctx std_context.Context, metrics otel_metrics.Metrics) error {
	payload, err := (&otel_metrics.ProtoMarshaler{}).MarshalMetrics(metrics)
	if err != nil {
		return std_fmt.Errorf("marshal metrics: %w", err)
	}
	fn, export := self.metricsExport()
	return self.deliver(ctx, fn, payload, export)
}

// ConsumeTraces: same shape as ConsumeLogs for the traces signal.
func (self *Component) ConsumeTraces(ctx std_context.Context, traces otel_traces.Traces) error {
	payload, err := (&otel_traces.ProtoMarshaler{}).MarshalTraces(traces)
	if err != nil {
		return std_fmt.Errorf("marshal traces: %w", err)
	}
	fn, export := self.tracesExport()
	return self.deliver(ctx, fn, payload, export)
}

// deliver runs the per-batch alloc → write → call → free dance and
// turns the guest's return code into a Go error. Shared by every
// ConsumeX so the marshal step is the only signal-specific code.
func (self *Component) deliver(ctx std_context.Context, fn wazero_api.Function, payload []byte, export string) error {
	rc, err := self.callConsume(ctx, fn, payload)
	if err != nil {
		self.guestLogger.Errorw(export+" failed", "error", err, "bytes", len(payload))
		return err
	}
	if rc != 0 {
		self.guestLogger.Warnw(export+" returned non-zero", "rc", rc, "bytes", len(payload))
		return std_fmt.Errorf("guest %s returned %d", export, rc)
	}
	return nil
}

// callConsume runs the per-batch sequence: alloc a guest-side buffer,
// write the payload into it, invoke the role's batch export, free the
// buffer. Holds callMu for the whole sequence — wazero modules are
// single-occupant and a partial sequence must not race with anything
// else entering the instance.
func (self *Component) callConsume(
	ctx std_context.Context,
	fn wazero_api.Function,
	payload []byte,
) (uint32, error) {
	size := uint64(len(payload))
	if size == 0 {
		return 0, nil
	}

	self.callMu.Lock()
	defer self.callMu.Unlock()
	if self.broken {
		return 0, std_errors.New("wasm4otel: component is poisoned (prior trap)")
	}

	allocRes, err := self.alloc.Call(ctx, size)
	if err != nil {
		self.broken = true
		return 0, std_fmt.Errorf("alloc trapped: %w", err)
	}
	ptr := uint32(allocRes[0])
	if ptr == 0 {
		// Guest signalled OOM; no buffer to free.
		return 0, std_fmt.Errorf("guest alloc returned 0 for %d bytes", size)
	}

	if !self.instance.Memory().Write(ptr, payload) {
		// Shouldn't happen if alloc succeeded; still try to release.
		if _, ferr := self.free.Call(ctx, uint64(ptr), size); ferr != nil {
			self.broken = true
		}
		return 0, std_fmt.Errorf("memory write out of range: ptr=%d size=%d", ptr, size)
	}

	consumeRes, err := fn.Call(ctx, uint64(ptr), size)
	if err != nil {
		// Don't try to free — instance state is undefined after a trap.
		self.broken = true
		return 0, std_fmt.Errorf("consume trapped: %w", err)
	}
	rc := uint32(consumeRes[0])

	if _, err := self.free.Call(ctx, uint64(ptr), size); err != nil {
		self.broken = true
		return rc, std_fmt.Errorf("free trapped (rc=%d): %w", rc, err)
	}

	return rc, nil
}
