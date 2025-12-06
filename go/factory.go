package wasm4otel

import (
	"context"
	"fmt"

	"go.opentelemetry.io/collector/component"
	"go.opentelemetry.io/collector/consumer"
	"go.opentelemetry.io/collector/pipeline"
	"go.opentelemetry.io/collector/receiver"
)

var (
	typeStr = component.MustNewType("wasm4otel")
)

// Conforms to the receiver.Factory interface which requires the methods:
// - CreateTraces
// - CreateMetrics
// - CreateLogs
// - TracesStability
// - MetricsStability
// - LogsStability
// - Type                  // from component.Factory
// - CreateDefaultConfig   // from component.Factory
//
// Not using receiver.factory is reinventing the wheel but I enjoy explicitness
// and dislike extraneous runtime layers and obfuscations
type Factory struct{}

func (self *Factory) Type() component.Type {
	return typeStr
}

func (self *Factory) CreateDefaultConfig() component.Config {
	return Config{
		Path: "",
	}
}

func (self *Factory) CreateTraces(context.Context, receiver.Settings, component.Config, consumer.Traces) (receiver.Traces, error) {
	return nil, pipeline.ErrSignalNotSupported
}

func (self *Factory) TracesStability() component.StabilityLevel {
	return component.StabilityLevelUndefined
}

func (self *Factory) CreateMetrics(context.Context, receiver.Settings, component.Config, consumer.Metrics) (receiver.Metrics, error) {
	return nil, pipeline.ErrSignalNotSupported
}

func (self *Factory) MetricsStability() component.StabilityLevel {
	return component.StabilityLevelUndefined
}

func (self *Factory) CreateLogs(ctx context.Context, settings receiver.Settings, config component.Config, next consumer.Logs) (receiver.Logs, error) {
	if settings.ID.Type() != self.Type() {
		return nil, fmt.Errorf("component type mismatch: component ID %q does not have type %q", settings.ID, self.Type())
	}
	return &WasmOtelLogsReceiver{}, nil
}

func (self *Factory) LogsStability() component.StabilityLevel {
	return component.StabilityLevelDevelopment
}

func NewFactory() receiver.Factory {
	return &Factory{}
}
