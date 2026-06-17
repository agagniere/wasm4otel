package wasm4otelexporter

import (
	std_context "context"
	std_errors "errors"

	otel_component "go.opentelemetry.io/collector/component"
	otel_exporter "go.opentelemetry.io/collector/exporter"

	wasm4otel "github.com/agagniere/wasm4otel/go"
)

// NewFactory returns the OTel exporter factory for wasm4otel plugins
// running in exporter mode: the host invokes the guest's consume_logs
// per incoming batch, and the guest is terminal — anything it tries
// to send via push_logs goes nowhere (NextConsumerLogs stays nil).
func NewFactory() otel_exporter.Factory {
	return otel_exporter.NewFactory(
		otel_component.MustNewType("wasm4otel"),
		wasm4otel.DefaultConfig,
		otel_exporter.WithLogs(createLogs, otel_component.StabilityLevelDevelopment),
	)
}

func createLogs(
	_ std_context.Context,
	settings otel_exporter.Settings,
	anyconfig otel_component.Config,
) (otel_exporter.Logs, error) {
	component, err := wasm4otel.NewComponent(anyconfig, settings.Logger)
	if err != nil {
		return nil, err
	}
	component.Mode = wasm4otel.ModeExporter
	if err = component.ExposeFunctionsToGuest(); err != nil {
		return nil, err
	}
	if err = component.LoadPlugin(); err != nil {
		return nil, err
	}
	if !component.HasConsumeLogs() {
		return nil, std_errors.New("wasm4otel exporter: plugin must export consume_logs, alloc, and free")
	}
	return component, nil
}
