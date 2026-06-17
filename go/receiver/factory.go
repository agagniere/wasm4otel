package wasm4otelreceiver

import (
	std_context "context"

	otel_component "go.opentelemetry.io/collector/component"
	otel_consumer "go.opentelemetry.io/collector/consumer"
	otel_receiver "go.opentelemetry.io/collector/receiver"

	wasm4otel "github.com/agagniere/wasm4otel/go"
)

// NewFactory returns the OTel receiver factory for wasm4otel plugins
// running in receiver mode: the plugin's start() drives a long-running
// loop and pushes telemetry via the push_logs host import.
func NewFactory() otel_receiver.Factory {
	return otel_receiver.NewFactory(
		otel_component.MustNewType("wasm4otel"),
		wasm4otel.DefaultConfig,
		otel_receiver.WithLogs(createLogs, otel_component.StabilityLevelDevelopment),
	)
}

func createLogs(
	_ std_context.Context,
	settings otel_receiver.Settings,
	anyconfig otel_component.Config,
	nextConsumer otel_consumer.Logs,
) (otel_receiver.Logs, error) {
	component, err := wasm4otel.NewComponent(anyconfig, settings.Logger)
	if err != nil {
		return nil, err
	}
	if err = component.ExposeFunctionsToGuest(); err != nil {
		return nil, err
	}
	if err = component.LoadPlugin(); err != nil {
		return nil, err
	}
	component.NextConsumerLogs = nextConsumer
	return component, nil
}
