package wasm4otelprocessor

import (
	std_context "context"
	std_errors "errors"

	otel_component "go.opentelemetry.io/collector/component"
	otel_consumer "go.opentelemetry.io/collector/consumer"
	otel_logs "go.opentelemetry.io/collector/pdata/plog"
	otel_metrics "go.opentelemetry.io/collector/pdata/pmetric"
	otel_traces "go.opentelemetry.io/collector/pdata/ptrace"
	otel_processor "go.opentelemetry.io/collector/processor"
	otel_helper "go.opentelemetry.io/collector/processor/processorhelper"

	wasm4otel "github.com/agagniere/wasm4otel/go"
)

// NewFactory returns the OTel processor factory for wasm4otel plugins
// running in processor mode: the host invokes the guest's
// wasm4otel_process_<signal> per incoming batch, and the guest forwards
// its transformed batch by calling the matching push_<signal> host
// import.
//
// Each signal is wrapped in processorhelper, which is what puts this
// component on the collector's internal telemetry:
// otelcol_processor_internal_duration times the Process<Signal> call,
// and otelcol_processor_{incoming,outgoing}_items count the records on
// each side of it. Getting a duration that means anything is why the
// guest's push is captured rather than forwarded — see
// Component.ProcessLogs.
func NewFactory() otel_processor.Factory {
	return otel_processor.NewFactory(
		otel_component.MustNewType("wasm4otel"),
		wasm4otel.DefaultConfig,
		otel_processor.WithLogs(createLogs, otel_component.StabilityLevelDevelopment),
		otel_processor.WithMetrics(createMetrics, otel_component.StabilityLevelDevelopment),
		otel_processor.WithTraces(createTraces, otel_component.StabilityLevelDevelopment),
	)
}

// options are the same for all three signals: the component owns the
// plugin's lifecycle, and it never mutates the caller's pdata — the
// batch is re-marshalled to OTLP bytes on the way into the guest, so
// what comes back is a fresh object. That second point matters enough
// to be explicit: processorhelper's default is MutatesData: true,
// which would have the collector clone every batch it fans out.
func options(component *wasm4otel.Component) []otel_helper.Option {
	return []otel_helper.Option{
		otel_helper.WithCapabilities(otel_consumer.Capabilities{MutatesData: false}),
		otel_helper.WithStart(component.Start),
		otel_helper.WithShutdown(component.Shutdown),
	}
}

// skipIfEmpty translates the host's "plugin pushed nothing" sentinel
// into the collector's. processorhelper reads ErrSkipProcessingData as
// a deliberate end of the road: the batch stops here, counted as zero
// outgoing items, and no error travels back up the pipeline.
func skipIfEmpty(err error) error {
	if std_errors.Is(err, wasm4otel.ErrNoOutput) {
		return otel_helper.ErrSkipProcessingData
	}
	return err
}

func createLogs(
	context std_context.Context,
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
	return otel_helper.NewLogs(context, settings, anyconfig, nextConsumer,
		func(ctx std_context.Context, logs otel_logs.Logs) (otel_logs.Logs, error) {
			out, err := component.ProcessLogs(ctx, logs)
			return out, skipIfEmpty(err)
		},
		options(component)...)
}

func createMetrics(
	context std_context.Context,
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
	return otel_helper.NewMetrics(context, settings, anyconfig, nextConsumer,
		func(ctx std_context.Context, metrics otel_metrics.Metrics) (otel_metrics.Metrics, error) {
			out, err := component.ProcessMetrics(ctx, metrics)
			return out, skipIfEmpty(err)
		},
		options(component)...)
}

func createTraces(
	context std_context.Context,
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
	return otel_helper.NewTraces(context, settings, anyconfig, nextConsumer,
		func(ctx std_context.Context, traces otel_traces.Traces) (otel_traces.Traces, error) {
			out, err := component.ProcessTraces(ctx, traces)
			return out, skipIfEmpty(err)
		},
		options(component)...)
}
