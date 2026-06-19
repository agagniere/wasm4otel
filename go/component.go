package wasm4otel

import (
	std_context "context"
	std_errors "errors"
	std_fmt "fmt"
	std_io "io"
	std_json "encoding/json"
	std_os "os"
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
	// ModeReceiver: start() drives a long-running loop on its own
	// goroutine; Shutdown cancels the context and joins it.
	ModeReceiver ComponentMode = iota
	// ModeProcessor: start() is a short-lived init; pipeline goroutines
	// drive the guest via ConsumeLogs/Metrics/Traces.
	ModeProcessor
	// ModeExporter: same lifecycle as ModeProcessor, but no downstream
	// consumer is wired — push_logs from the guest goes nowhere.
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
	// get_config_size / get_config host imports.
	pluginConfigJSON []byte

	start          wazero_api.Function
	stop           wazero_api.Function
	consumeLogs    wazero_api.Function
	consumeMetrics wazero_api.Function
	consumeTraces  wazero_api.Function
	alloc          wazero_api.Function
	free           wazero_api.Function

	NextConsumerLogs    otel_consumer.Logs
	NextConsumerMetrics otel_consumer.Metrics
	NextConsumerTraces  otel_consumer.Traces

	startWg std_sync.WaitGroup

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
// registers the host imports, loads the plugin from disk, and verifies
// the exports the mode requires regardless of pipeline signal — today
// the wasm4otel_alloc / wasm4otel_free pair, skipped for ModeReceiver
// since receivers don't consume batches. Signal-specific validation
// (e.g. ValidateLogsExport) is the caller's responsibility.
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
	if mode != ModeReceiver && !component.HasAllocFree() {
		return nil, std_fmt.Errorf("wasm4otel %s: plugin must export wasm4otel_alloc and wasm4otel_free", mode)
	}
	return component, nil
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
	if self.start == nil {
		return nil
	}
	switch self.mode {
	case ModeReceiver:
		// Long-running loop owns the instance until Shutdown cancels it.
		// Runs without callMu — the goroutine effectively holds the
		// instance for its lifetime, and Shutdown waits for it to drain
		// before calling stop.
		self.startWg.Add(1)
		go func() {
			defer self.startWg.Done()
			if _, err := self.start.Call(self.context); err != nil {
				self.guestLogger.Warnw("start returned with error", "error", err)
			}
		}()
	case ModeProcessor, ModeExporter:
		// Synchronous init; must return promptly so the pipeline can
		// start delivering batches via ConsumeLogs.
		if _, err := self.invoke(self.context, self.start); err != nil {
			self.guestLogger.Warnw("start returned with error", "error", err)
			return err
		}
	}
	return nil
}

func (self *Component) Shutdown(context std_context.Context) error {
	// Cancel the plugin's context first so any blocking host import
	// (interruptible_sleep_ms, push_logs) returns to the guest with a
	// cancellation signal; the plugin's start loop unwinds on its own,
	// then we wait. Processor/exporter modes have no goroutine to wait
	// on, so the Wait is a no-op there.
	self.cancel()
	if self.mode == ModeReceiver {
		self.startWg.Wait()
	}
	if self.stop != nil {
		if _, err := self.invoke(context, self.stop); err != nil {
			self.guestLogger.Warnw("stop returned with error", "error", err)
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

	config := wazero.NewModuleConfig().
		WithStartFunctions("_start", "_initialize").
		WithSysWalltime().
		WithSysNanotime()
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

	self.instance = instance
	self.start = instance.ExportedFunction("start")
	self.stop = instance.ExportedFunction("stop")

	self.consumeLogs = instance.ExportedFunction("consume_logs")
	self.consumeMetrics = instance.ExportedFunction("consume_metrics")
	self.consumeTraces = instance.ExportedFunction("consume_traces")
	// Names are prefixed because Rust + wasm32-wasi links wasi-libc,
	// which already defines `free`; an unprefixed export collides at
	// link time. The Zig path doesn't link libc and would survive
	// either name, but we use the same names everywhere for symmetry.
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

// HasConsumeLogs reports whether the guest exports consume_logs.
// Pair with HasAllocFree to know whether the logs path is wireable.
func (self *Component) HasConsumeLogs() bool {
	return self.consumeLogs != nil
}

// ValidateLogsExport reports whether the guest can serve the logs
// signal in this component's mode. Used by processor/exporter
// factories' createLogs hooks; receivers don't consume so they don't
// call this.
func (self *Component) ValidateLogsExport() error {
	if !self.HasConsumeLogs() {
		return std_fmt.Errorf("wasm4otel %s: plugin does not export consume_logs; it does not support the logs signal", self.mode)
	}
	return nil
}

// HasConsumeMetrics reports whether the guest exports consume_metrics.
func (self *Component) HasConsumeMetrics() bool {
	return self.consumeMetrics != nil
}

// ValidateMetricsExport reports whether the guest can serve the
// metrics signal in this component's mode.
func (self *Component) ValidateMetricsExport() error {
	if !self.HasConsumeMetrics() {
		return std_fmt.Errorf("wasm4otel %s: plugin does not export consume_metrics; it does not support the metrics signal", self.mode)
	}
	return nil
}

// HasConsumeTraces reports whether the guest exports consume_traces.
func (self *Component) HasConsumeTraces() bool {
	return self.consumeTraces != nil
}

// ValidateTracesExport reports whether the guest can serve the traces
// signal in this component's mode.
func (self *Component) ValidateTracesExport() error {
	if !self.HasConsumeTraces() {
		return std_fmt.Errorf("wasm4otel %s: plugin does not export consume_traces; it does not support the traces signal", self.mode)
	}
	return nil
}

// Capabilities satisfies consumer.Logs/Metrics/Traces. We always
// re-marshal the incoming pdata to bytes before crossing into the
// guest, so we never mutate the caller's view.
func (self *Component) Capabilities() otel_consumer.Capabilities {
	return otel_consumer.Capabilities{MutatesData: false}
}

// ConsumeLogs marshals the batch to OTLP bytes, hands the bytes to the
// guest via the alloc → write → consume_logs → free sequence, all under
// callMu. The guest forwards its transformed batch via the push_logs
// host import — that path uses NextConsumerLogs, not the return path.
func (self *Component) ConsumeLogs(ctx std_context.Context, logs otel_logs.Logs) error {
	payload, err := (&otel_logs.ProtoMarshaler{}).MarshalLogs(logs)
	if err != nil {
		return std_fmt.Errorf("marshal logs: %w", err)
	}
	return self.deliver(ctx, self.consumeLogs, payload, "consume_logs")
}

// ConsumeMetrics: same shape as ConsumeLogs for the metrics signal.
func (self *Component) ConsumeMetrics(ctx std_context.Context, metrics otel_metrics.Metrics) error {
	payload, err := (&otel_metrics.ProtoMarshaler{}).MarshalMetrics(metrics)
	if err != nil {
		return std_fmt.Errorf("marshal metrics: %w", err)
	}
	return self.deliver(ctx, self.consumeMetrics, payload, "consume_metrics")
}

// ConsumeTraces: same shape as ConsumeLogs for the traces signal.
func (self *Component) ConsumeTraces(ctx std_context.Context, traces otel_traces.Traces) error {
	payload, err := (&otel_traces.ProtoMarshaler{}).MarshalTraces(traces)
	if err != nil {
		return std_fmt.Errorf("marshal traces: %w", err)
	}
	return self.deliver(ctx, self.consumeTraces, payload, "consume_traces")
}

// deliver runs the per-batch alloc → write → consume → free dance and
// turns the guest's return code into a Go error. Shared by every
// ConsumeX so the marshal step is the only signal-specific code.
func (self *Component) deliver(ctx std_context.Context, fn wazero_api.Function, payload []byte, signal string) error {
	rc, err := self.callConsume(ctx, fn, payload)
	if err != nil {
		self.guestLogger.Errorw(signal+" failed", "error", err, "bytes", len(payload))
		return err
	}
	if rc != 0 {
		self.guestLogger.Warnw(signal+" returned non-zero", "rc", rc, "bytes", len(payload))
		return std_fmt.Errorf("guest %s returned %d", signal, rc)
	}
	return nil
}

// callConsume runs the per-batch sequence: alloc a guest-side buffer,
// write the payload into it, invoke the consume_* export, free the
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
