package wasm4otelreceiver

import (
	std_context "context"

	otel_component "go.opentelemetry.io/collector/component"
	otel_consumer "go.opentelemetry.io/collector/consumer"
	otel_receiver "go.opentelemetry.io/collector/receiver"

	wasm4otel "github.com/agagniere/wasm4otel/go/wazero"
)

// NewFactory returns the OTel receiver factory for wasm4otel plugins
// running in receiver mode: the plugin's wasm4otel_receive drives a
// long-running loop and pushes telemetry via the push_<signal> host
// imports. Receivers are signal-agnostic at the entry point — one loop
// serves whichever signals the plugin pushes — so all three createX
// hooks require the same single export.
//
// LoadReceiver also attaches receiverhelper's ObsReport, which is what
// puts this component on otelcol_receiver_{accepted,refused,failed}_*
// and starts the span every downstream helper hangs its own
// instrumentation off — see Component.pushLogs.
func NewFactory() otel_receiver.Factory {
	return otel_receiver.NewFactory(
		otel_component.MustNewType("wasm4otel"),
		wasm4otel.DefaultConfig,
		otel_receiver.WithLogs(createLogs, otel_component.StabilityLevelDevelopment),
		otel_receiver.WithMetrics(createMetrics, otel_component.StabilityLevelDevelopment),
		otel_receiver.WithTraces(createTraces, otel_component.StabilityLevelDevelopment),
	)
}

func createLogs(
	_ std_context.Context,
	settings otel_receiver.Settings,
	anyconfig otel_component.Config,
	nextConsumer otel_consumer.Logs,
) (otel_receiver.Logs, error) {
	component, err := wasm4otel.LoadReceiver(anyconfig, settings)
	if err != nil {
		return nil, err
	}
	component.NextConsumerLogs = nextConsumer
	return component, nil
}

func createMetrics(
	_ std_context.Context,
	settings otel_receiver.Settings,
	anyconfig otel_component.Config,
	nextConsumer otel_consumer.Metrics,
) (otel_receiver.Metrics, error) {
	component, err := wasm4otel.LoadReceiver(anyconfig, settings)
	if err != nil {
		return nil, err
	}
	component.NextConsumerMetrics = nextConsumer
	return component, nil
}

func createTraces(
	_ std_context.Context,
	settings otel_receiver.Settings,
	anyconfig otel_component.Config,
	nextConsumer otel_consumer.Traces,
) (otel_receiver.Traces, error) {
	component, err := wasm4otel.LoadReceiver(anyconfig, settings)
	if err != nil {
		return nil, err
	}
	component.NextConsumerTraces = nextConsumer
	return component, nil
}
