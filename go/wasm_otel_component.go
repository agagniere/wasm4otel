package wasm4otel

import (
	std_context "context"
	std_fmt "fmt"
	std_io "io"
	std_os "os"

	uber_zap "go.uber.org/zap"
	uber_zapcore "go.uber.org/zap/zapcore"

	otel_component "go.opentelemetry.io/collector/component"
	otel_consumer "go.opentelemetry.io/collector/consumer"
	otel_logs "go.opentelemetry.io/collector/pdata/plog"

	"github.com/tetratelabs/wazero"
	wazero_api "github.com/tetratelabs/wazero/api"
	wazero_sys "github.com/tetratelabs/wazero/sys"
	//"github.com/tetratelabs/wazero/imports/wasi_snapshot_preview1"
)

type WasmOtelComponent struct {
	logger           *uber_zap.SugaredLogger
	guestLogger      *uber_zap.SugaredLogger
	context          std_context.Context
	cancel           std_context.CancelFunc
	config           Config
	runtime          wazero.Runtime
	start            wazero_api.Function
	stop             wazero_api.Function
	capabilities     wazero_api.Function
	consumeLogs      wazero_api.Function
	consumeMetrics   wazero_api.Function
	consumeTraces    wazero_api.Function
	nextConsumerLogs otel_consumer.Logs
}

func NewWasmOtelComponent(
	context std_context.Context,
	anyconfig otel_component.Config,
	logger *uber_zap.Logger,
) (WasmOtelComponent, error) {
	hostLogger := logger.Sugar()
	config := anyconfig.(Config)
	if err := config.Validate(); err != nil {
		return WasmOtelComponent{}, err
	}
	hostLogger.Infow("Loading WebAssembly plugin", "path", config.Path)
	context, cancel := std_context.WithCancel(context)
	runtime := newRuntime(context)
	return WasmOtelComponent{
		logger:  hostLogger,
		context: context,
		cancel:  cancel,
		config:  config,
		runtime: runtime,
	}, nil
}

func newRuntime(context std_context.Context) wazero.Runtime {
	runtime := wazero.NewRuntimeWithConfig(context, wazero.NewRuntimeConfigInterpreter())
	//wasi_snapshot_preview1.MustInstantiate(context, runtime)
	return runtime
}

// Provide a callback accessible from the guest to log
// and functions to push logs/metrics/traces to the next consumer
func (self *WasmOtelComponent) ExposeFunctionsToGuest() error {
	self.guestLogger = self.logger.With("plugin", self.config.Path)
	_, err := self.runtime.NewHostModuleBuilder("env").
		NewFunctionBuilder().WithFunc(self.logToZap).Export("host_log").
		NewFunctionBuilder().WithFunc(self.outboundLogs).Export("push_logs").
		//NewFunctionBuilder().WithFunc(self.outboundMetrics).Export("push_metrics").
		//NewFunctionBuilder().WithFunc(self.outboundTraces).Export("push_traces").
		Instantiate(self.context)
	return err
}

func (self *WasmOtelComponent) outboundLogs(
	_ std_context.Context,
	module wazero_api.Module,
	offset uint32,
	size uint32,
) {
	buffer, ok := module.Memory().Read(offset, size)
	if !ok {
		self.guestLogger.Errorf("Unable to read (%d, %d) from memory", offset, size)
		return
	}
	deserializer := otel_logs.ProtoUnmarshaler{}
	logs, err := deserializer.UnmarshalLogs(buffer)
	if err != nil {
		self.guestLogger.Errorw("Unable to deserialize logs", "size", size)
		return
	}
	if self.nextConsumerLogs == nil {
		self.guestLogger.Error("Plugin is pushing logs to a dead-end")
		return
	}
	self.nextConsumerLogs.ConsumeLogs(self.context, logs)
}

func (self *WasmOtelComponent) Start(context std_context.Context, host otel_component.Host) error {
	if self.start != nil {
		self.start.Call(context)
	}
	return nil
}

func (self *WasmOtelComponent) Shutdown(context std_context.Context) error {
	if self.start != nil {
		self.stop.Call(context)
	}
	self.cancel()
	return nil
}

func (self *WasmOtelComponent) logToZap(_ std_context.Context, module wazero_api.Module, level int32, offset uint32, size uint32) {
	buffer, ok := module.Memory().Read(offset, size)
	if !ok {
		self.guestLogger.Errorf("Unable to read (%d, %d) from memory", offset, size)
		return
	}
	self.guestLogger.Log(uber_zapcore.Level(level), string(buffer))
}

func (self *WasmOtelComponent) LoadPlugin() error {
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

	config := wazero.NewModuleConfig()
	//WithStdout(std_os.Stdout).
	//WithStderr(std_os.Stderr)
	//WithStartFunctions("_initialize").
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

	self.start = instance.ExportedFunction("start")
	self.stop = instance.ExportedFunction("stop")
	self.capabilities = instance.ExportedFunction("capabilities")
	self.consumeLogs = instance.ExportedFunction("consume_logs")
	self.consumeMetrics = instance.ExportedFunction("consume_metrics")
	self.consumeTraces = instance.ExportedFunction("consume_traces")
	return nil
}
