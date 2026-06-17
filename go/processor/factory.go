package wasm4otelprocessor

import (
	std_context "context"
	std_fmt "fmt"

	otel_component "go.opentelemetry.io/collector/component"
	otel_consumer "go.opentelemetry.io/collector/consumer"
	otel_processor "go.opentelemetry.io/collector/processor"

	wasm4otel "github.com/agagniere/wasm4otel/go"
)

// NewFactory returns the OTel processor factory for wasm4otel plugins
// running in processor mode: the host invokes the guest's consume_<signal>
// per incoming batch, and the guest forwards its transformed batch
// downstream via the matching push_<signal> host import.
func NewFactory() otel_processor.Factory {
	return otel_processor.NewFactory(
		otel_component.MustNewType("wasm4otel"),
		wasm4otel.DefaultConfig,
		otel_processor.WithLogs(createLogs, otel_component.StabilityLevelDevelopment),
	)
}

// loadComponent runs the signal-independent setup every createX shares:
// instantiate, expose host imports, load the wasm, and verify the
// alloc/free pair the consume path always needs. The caller adds the
// signal-specific consume_<signal> check on top.
func loadComponent(anyconfig otel_component.Config, settings otel_processor.Settings) (*wasm4otel.Component, error) {
	component, err := wasm4otel.NewComponent(anyconfig, settings.Logger, wasm4otel.ModeProcessor)
	if err != nil {
		return nil, err
	}
	if err = component.ExposeFunctionsToGuest(); err != nil {
		return nil, err
	}
	if err = component.LoadPlugin(); err != nil {
		return nil, err
	}
	if !component.HasAllocFree() {
		return nil, std_fmt.Errorf("wasm4otel processor: plugin must export wasm4otel_alloc and wasm4otel_free")
	}
	return component, nil
}

func createLogs(
	_ std_context.Context,
	settings otel_processor.Settings,
	anyconfig otel_component.Config,
	nextConsumer otel_consumer.Logs,
) (otel_processor.Logs, error) {
	component, err := loadComponent(anyconfig, settings)
	if err != nil {
		return nil, err
	}
	if !component.HasConsumeLogs() {
		return nil, std_fmt.Errorf("wasm4otel processor: plugin does not export consume_logs; it does not support the logs signal")
	}
	component.NextConsumerLogs = nextConsumer
	return component, nil
}
