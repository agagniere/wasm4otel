package wasm4otel

import (
	"context"
	"go.opentelemetry.io/collector/component"
)

type WasmOtelLogsReceiver struct{}

func (self *WasmOtelLogsReceiver) Start(ctx context.Context, host component.Host) error {
	return nil
}

func (self *WasmOtelLogsReceiver) Shutdown(ctx context.Context) error {
	return nil
}
