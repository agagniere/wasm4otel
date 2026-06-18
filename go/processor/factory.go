package wasm4otelprocessor

import (
	std_context "context"

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
		otel_processor.WithMetrics(createMetrics, otel_component.StabilityLevelDevelopment),
		otel_processor.WithTraces(createTraces, otel_component.StabilityLevelDevelopment),
	)
}

func createLogs(
	_ std_context.Context,
	settings otel_processor.Settings,
	anyconfig otel_component.Config,
	nextConsumer otel_consumer.Logs,
) (otel_processor.Logs, error) {
	component, err := wasm4otel.Load(anyconfig, settings.Logger, wasm4otel.ModeProcessor)
	if err != nil {
		return nil, err
	}
	if err := component.ValidateLogsExport(); err != nil {
		return nil, err
	}
	component.NextConsumerLogs = nextConsumer
	return component, nil
}

func createMetrics(
	_ std_context.Context,
	settings otel_processor.Settings,
	anyconfig otel_component.Config,
	nextConsumer otel_consumer.Metrics,
) (otel_processor.Metrics, error) {
	component, err := wasm4otel.Load(anyconfig, settings.Logger, wasm4otel.ModeProcessor)
	if err != nil {
		return nil, err
	}
	if err := component.ValidateMetricsExport(); err != nil {
		return nil, err
	}
	component.NextConsumerMetrics = nextConsumer
	return component, nil
}

func createTraces(
	_ std_context.Context,
	settings otel_processor.Settings,
	anyconfig otel_component.Config,
	nextConsumer otel_consumer.Traces,
) (otel_processor.Traces, error) {
	component, err := wasm4otel.Load(anyconfig, settings.Logger, wasm4otel.ModeProcessor)
	if err != nil {
		return nil, err
	}
	if err := component.ValidateTracesExport(); err != nil {
		return nil, err
	}
	component.NextConsumerTraces = nextConsumer
	return component, nil
}
