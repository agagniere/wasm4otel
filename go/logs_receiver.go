package wasm4otel

import (
	"context"
	"go.opentelemetry.io/collector/component"
	"go.uber.org/zap"
)

type WasmOtelLogsReceiver struct {
	logger *zap.SugaredLogger
}

func (self *WasmOtelLogsReceiver) Start(ctx context.Context, host component.Host) error {
	self.logger.Info("Starting")
	return nil
}

func (self *WasmOtelLogsReceiver) Shutdown(ctx context.Context) error {
	self.logger.Info("Stopping")
	return nil
}
