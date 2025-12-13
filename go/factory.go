package wasm4otel

import (
	"context"

	"go.opentelemetry.io/collector/component"
	"go.opentelemetry.io/collector/consumer"
	"go.opentelemetry.io/collector/receiver"
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
func NewFactory() receiver.Factory {
	return receiver.NewFactory(
		component.MustNewType("wasm4otel"),
		DefaultConfig,
		receiver.WithLogs(createLogs, component.StabilityLevelDevelopment),
	)
}

func createLogs(
	ctx context.Context,
	settings receiver.Settings,
	anyconfig component.Config,
	nextConsumer consumer.Logs,
) (receiver.Logs, error) {
	logger := settings.Logger.Sugar()
	logger.Infow("Instanciate logs exporter",
		"ID", settings.ID,
		"build info", settings.BuildInfo)
	config := anyconfig.(*Config)
	if err := config.Validate(); err != nil {
		return nil, err
	}
	return &WasmOtelLogsReceiver{
		logger: logger,
	}, nil
}
