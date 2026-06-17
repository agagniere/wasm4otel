package wasm4otel

import (
	std_context "context"
	std_errors "errors"
	std_fmt "fmt"
	std_io "io"
	std_os "os"
	std_sync "sync"
	std_time "time"

	uber_zap "go.uber.org/zap"
	uber_zapcore "go.uber.org/zap/zapcore"

	otel_component "go.opentelemetry.io/collector/component"
	otel_consumer "go.opentelemetry.io/collector/consumer"
	otel_logs "go.opentelemetry.io/collector/pdata/plog"

	"github.com/tetratelabs/wazero"
	wazero_api "github.com/tetratelabs/wazero/api"
	wazero_sys "github.com/tetratelabs/wazero/sys"
	"github.com/tetratelabs/wazero/imports/wasi_snapshot_preview1"
)

// ComponentMode selects the lifecycle a Component runs under. It is
// set by the role-specific factory immediately after NewComponent.
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

type Component struct {
	Mode ComponentMode

	logger      *uber_zap.SugaredLogger
	guestLogger *uber_zap.SugaredLogger
	context     std_context.Context
	cancel      std_context.CancelFunc
	config      Config
	runtime     wazero.Runtime
	instance    wazero_api.Module

	start          wazero_api.Function
	stop           wazero_api.Function
	consumeLogs    wazero_api.Function
	consumeMetrics wazero_api.Function
	consumeTraces  wazero_api.Function
	alloc          wazero_api.Function
	free           wazero_api.Function

	NextConsumerLogs otel_consumer.Logs

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

func NewComponent(
	anyconfig otel_component.Config,
	logger *uber_zap.Logger,
) (*Component, error) {
	hostLogger := logger.Sugar()
	config := anyconfig.(Config)
	if err := config.Validate(); err != nil {
		return nil, err
	}
	hostLogger.Infow("Loading WebAssembly plugin", "path", config.Path)
	// Detach from the framework's create-phase ctx — the component
	// owns its own cancellation, ended only by Shutdown via `cancel`.
	context, cancel := std_context.WithCancel(std_context.Background())
	runtime := newRuntime(context)
	return &Component{
		logger:  hostLogger,
		context: context,
		cancel:  cancel,
		config:  config,
		runtime: runtime,
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
		//NewFunctionBuilder().WithFunc(self.outboundMetrics).Export("push_metrics").
		//NewFunctionBuilder().WithFunc(self.outboundTraces).Export("push_traces").
		NewFunctionBuilder().WithFunc(self.interruptibleSleepMs).Export("interruptible_sleep_ms").
		Instantiate(self.context)
	return err
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
	self.NextConsumerLogs.ConsumeLogs(self.context, logs)
	self.guestLogger.Infow("OK", "bytes", size)
	return 0
}

func (self *Component) Start(_ std_context.Context, _ otel_component.Host) error {
	if self.start == nil {
		return nil
	}
	switch self.Mode {
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
	if self.Mode == ModeReceiver {
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

// HasConsumeMetrics reports whether the guest exports consume_metrics.
func (self *Component) HasConsumeMetrics() bool {
	return self.consumeMetrics != nil
}

// HasConsumeTraces reports whether the guest exports consume_traces.
func (self *Component) HasConsumeTraces() bool {
	return self.consumeTraces != nil
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
	serializer := otel_logs.ProtoMarshaler{}
	payload, err := serializer.MarshalLogs(logs)
	if err != nil {
		return std_fmt.Errorf("marshal logs: %w", err)
	}
	rc, err := self.callConsume(ctx, self.consumeLogs, payload)
	if err != nil {
		self.guestLogger.Errorw("consume_logs failed", "error", err, "bytes", len(payload))
		return err
	}
	if rc != 0 {
		self.guestLogger.Warnw("consume_logs returned non-zero", "rc", rc, "bytes", len(payload))
		return std_fmt.Errorf("guest consume_logs returned %d", rc)
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
	if fn == nil {
		return 0, std_errors.New("wasm4otel: guest does not export the requested consume function")
	}
	if self.alloc == nil || self.free == nil {
		return 0, std_errors.New("wasm4otel: guest does not export wasm4otel_alloc/wasm4otel_free")
	}
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
