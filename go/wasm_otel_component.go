package wasm4otel

import (
	std_context "context"
	std_fmt "fmt"
	std_io "io"
	std_os "os"

	uber_zap "go.uber.org/zap"
	uber_zapcore "go.uber.org/zap/zapcore"

	otel_component "go.opentelemetry.io/collector/component"

	"github.com/tetratelabs/wazero"
	wazero_api "github.com/tetratelabs/wazero/api"
	wazero_sys "github.com/tetratelabs/wazero/sys"
)

type WasmOtelComponent struct {
	logger      *uber_zap.SugaredLogger
	guestLogger *uber_zap.SugaredLogger
	context     std_context.Context
	cancel      std_context.CancelFunc
	config      Config
	runtime     wazero.Runtime
}

func NewWasmOtelComponent(
	context std_context.Context,
	anyconfig otel_component.Config,
	logger *uber_zap.Logger,
	identifier otel_component.ID,
) (WasmOtelComponent, error) {
	hostLogger := logger.Sugar()
	hostLogger.Infow("Instanciate WebAssembly component", "ID", identifier)

	config := anyconfig.(Config)
	if err := config.Validate(); err != nil {
		return WasmOtelComponent{}, err
	}
	context, cancel := std_context.WithCancel(context)
	runtime := newRuntime(context)
	return WasmOtelComponent{
		logger:      hostLogger,
		guestLogger: hostLogger.Named(identifier.String()),
		context:     context,
		cancel:      cancel,
		config:      config,
		runtime:     runtime,
	}, nil
}

func (self *WasmOtelComponent) Start(context std_context.Context, host otel_component.Host) error {
	self.startRuntime()
	return nil
}

func (self *WasmOtelComponent) Shutdown(ctx std_context.Context) error {
	self.cancel()
	return nil
}

func (self *WasmOtelComponent) logToZap(_ std_context.Context, module wazero_api.Module, level int32, offset uint32, size uint32) {
	buffer, ok := module.Memory().Read(offset, size)
	if !ok {
		self.logger.Errorf("Unable to read (%d, %d) from memory", offset, size)
		return
	}
	self.guestLogger.Log(uber_zapcore.Level(level), string(buffer))
}

func (self *WasmOtelComponent) startRuntime() error {
	_, err := self.runtime.NewHostModuleBuilder("env").
		NewFunctionBuilder().WithFunc(self.logToZap).Export("host_log").
		Instantiate(self.context)
	if err != nil {
		self.logger.Errorw("Unable to instanciate host module",
			"error", err)
	}

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

	target := "start"
	recvLogs := instance.ExportedFunction(target)
	if recvLogs == nil {
		self.logger.Errorw("Couldn't find function in module",
			"function", target,
			"path", self.config.Path)
		return std_fmt.Errorf("function not found: %s", target)
	}
	result, err := recvLogs.Call(self.context)
	if err != nil {
		self.logger.Errorw("Unable to call", "function", target)
		return err
	}
	self.logger.Infow("OK", "status", result)
	return nil
}
