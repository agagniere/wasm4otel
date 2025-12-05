package wasm4otel

import (
	"context"
	"time"

	"go.opentelemetry.io/collector/component"
	"go.opentelemetry.io/collector/consumer"
	"go.opentelemetry.io/collector/pipeline"
	"go.opentelemetry.io/collector/receiver"
	receiverinternal "go.opentelemetry.io/collector/receiver/internal"
)

const (
	typeStr = component.MustNewType("wasm4otel")
)

// Conforms to the receiver.Factory interface which requires the methods:
// - CreateTraces
// - CreateMetrics
// - CreateLogs
// - TracesStability
// - MetricsStability
// - LogsStability
// - unexportedFactoryFunc !!???
//
// Not using receiver.factory is reinventing the wheel but I enjoy explicitness and dislike extraneous runtime layers
type Factory struct {}

func (self *Factory) CreateTraces(context.Context, receiver.Settings, component.Config, consumer.Traces) (receiver.Traces, error) {
	return nil, pipeline.ErrSignalNotSupported
}

func (self *Factory) TracesStability() component.StabilityLevel {
	return StabilityLevelUndefined
}

func (self *Factory) CreateMetrics(context.Context, receiver.Settings, component.Config, consumer.Metrics) (receiver.Metrics, error) {
	return nil, pipeline.ErrSignalNotSupported
}

func (self *Factory) MetricsStability() component.StabilityLevel {
	return StabilityLevelUndefined
}

func (self *Factory) CreateLogs(ctx context.Context, settings receiver.Settings, config component.Config, next consumer.Logs) (receiver.Logs, error) {
	if settings.ID.Type() != typeStr {
		return nil, receiverinternal.ErrIDMismatch(settings.ID, typeStr)
	}
	return nil, pipeline.ErrSignalNotSupported
}

func (self *Factory) LogsStability() component.StabilityLevel {
	return 	StabilityLevelDevelopment
}

func NewFactory() receiver.Factory {
	return Factory{}
}
