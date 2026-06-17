package wasm4otelprocessor

import (
	std_context "context"
	std_errors "errors"

	otel_component "go.opentelemetry.io/collector/component"
	otel_consumer "go.opentelemetry.io/collector/consumer"
	otel_processor "go.opentelemetry.io/collector/processor"

	wasm4otel "github.com/agagniere/wasm4otel/go"
)

// NewFactory returns the OTel processor factory for wasm4otel plugins
// running in processor mode: the host invokes the guest's consume_logs
// per incoming batch, and the guest forwards its transformed batch
// downstream via the push_logs host import.
func NewFactory() otel_processor.Factory {
	return otel_processor.NewFactory(
		otel_component.MustNewType("wasm4otel"),
		wasm4otel.DefaultConfig,
		otel_processor.WithLogs(createLogs, otel_component.StabilityLevelDevelopment),
	)
}

func createLogs(
	_ std_context.Context,
	settings otel_processor.Settings,
	anyconfig otel_component.Config,
	nextConsumer otel_consumer.Logs,
) (otel_processor.Logs, error) {
	component, err := wasm4otel.NewComponent(anyconfig, settings.Logger)
	if err != nil {
		return nil, err
	}
	component.Mode = wasm4otel.ModeProcessor
	if err = component.ExposeFunctionsToGuest(); err != nil {
		return nil, err
	}
	if err = component.LoadPlugin(); err != nil {
		return nil, err
	}
	if !component.HasConsumeLogs() {
		return nil, std_errors.New("wasm4otel processor: plugin must export consume_logs, alloc, and free")
	}
	component.NextConsumerLogs = nextConsumer
	return component, nil
}
