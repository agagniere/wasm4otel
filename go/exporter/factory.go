package wasm4otelexporter

import (
	std_context "context"

	otel_component "go.opentelemetry.io/collector/component"
	otel_exporter "go.opentelemetry.io/collector/exporter"

	wasm4otel "github.com/agagniere/wasm4otel/go"
)

// NewFactory returns the OTel exporter factory for wasm4otel plugins
// running in exporter mode: the host invokes the guest's consume_<signal>
// per incoming batch, and the guest is terminal — anything it tries
// to push via push_<signal> goes nowhere.
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
	component, err := wasm4otel.Load(anyconfig, settings.Logger, wasm4otel.ModeExporter)
	if err != nil {
		return nil, err
	}
	if err := component.ValidateLogsExport(); err != nil {
		return nil, err
	}
	return component, nil
}
