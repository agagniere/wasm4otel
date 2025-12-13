package wasm4otel

import (
	std_context "context"

	otel_component "go.opentelemetry.io/collector/component"
	otel_consumer "go.opentelemetry.io/collector/consumer"
	otel_receiver "go.opentelemetry.io/collector/receiver"
)

// The receiver.Factory interface requires the following methods:
// - CreateTraces
// - CreateMetrics
// - CreateLogs
// - TracesStability
// - MetricsStability
// - LogsStability
// - Type                  // from component.Factory
// - CreateDefaultConfig   // from component.Factory
//
// But it cannot be implemented outside of the receiver package, because it also requires
// an unexported method, to force us to use receiver.NewFactory
func NewFactory() otel_receiver.Factory {
	return otel_receiver.NewFactory(
		otel_component.MustNewType("wasm4otel"),
		DefaultConfig,
		otel_receiver.WithLogs(createLogs, otel_component.StabilityLevelDevelopment),
	)
}

func createLogs(
	context std_context.Context,
	settings otel_receiver.Settings,
	anyconfig otel_component.Config,
	nextConsumer otel_consumer.Logs,
) (otel_receiver.Logs, error) {
	logger := settings.Logger.Sugar()
	logger.Infow("Instanciate logs exporter",
		"ID", settings.ID,
		"build info", settings.BuildInfo)
	config := anyconfig.(Config)
	if err := config.Validate(); err != nil {
		return nil, err
	}
	context, cancel := std_context.WithCancel(std_context.Background())
	return &WasmOtelLogsReceiver{
		logger:  logger,
		sink:    nextConsumer,
		context: context,
		cancel:  cancel,
		config:  config,
	}, nil
}
