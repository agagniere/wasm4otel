package wasm4otelexporter

import (
	std_context "context"
	std_fmt "fmt"

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

// loadComponent runs the signal-independent setup every createX shares:
// instantiate, expose host imports, load the wasm, and verify the
// alloc/free pair the consume path always needs. The caller adds the
// signal-specific consume_<signal> check on top.
func loadComponent(anyconfig otel_component.Config, settings otel_exporter.Settings) (*wasm4otel.Component, error) {
	component, err := wasm4otel.NewComponent(anyconfig, settings.Logger, wasm4otel.ModeExporter)
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
		return nil, std_fmt.Errorf("wasm4otel exporter: plugin must export wasm4otel_alloc and wasm4otel_free")
	}
	return component, nil
}

func createLogs(
	_ std_context.Context,
	settings otel_exporter.Settings,
	anyconfig otel_component.Config,
) (otel_exporter.Logs, error) {
	component, err := loadComponent(anyconfig, settings)
	if err != nil {
		return nil, err
	}
	if !component.HasConsumeLogs() {
		return nil, std_fmt.Errorf("wasm4otel exporter: plugin does not export consume_logs; it does not support the logs signal")
	}
	return component, nil
}
