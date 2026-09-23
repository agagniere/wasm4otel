package wasm4otelexporter

import (
	std_context "context"

	otel_component "go.opentelemetry.io/collector/component"
	otel_exporter "go.opentelemetry.io/collector/exporter"
	otel_helper "go.opentelemetry.io/collector/exporter/exporterhelper"

	wasm4otel "github.com/agagniere/wasm4otel/go"
)

// NewFactory returns the OTel exporter factory for wasm4otel plugins
// running in exporter mode: the host invokes the guest's
// wasm4otel_export_<signal> per incoming batch, and the guest is
// terminal — anything it tries to push via push_<signal> goes nowhere.
//
// Each signal is wrapped in exporterhelper, which is what puts this
// component on the collector's internal telemetry:
// otelcol_exporter_sent_<items> and
// otelcol_exporter_send_failed_<items> split the batch by outcome,
// otelcol_exporter_in_flight_requests counts the calls currently
// inside the guest, and each call gets an exporter/<id>/<signal> span.
func NewFactory() otel_exporter.Factory {
	return otel_exporter.NewFactory(
		otel_component.MustNewType("wasm4otel"),
		wasm4otel.DefaultConfig,
		otel_exporter.WithLogs(createLogs, otel_component.StabilityLevelDevelopment),
		otel_exporter.WithMetrics(createMetrics, otel_component.StabilityLevelDevelopment),
		otel_exporter.WithTraces(createTraces, otel_component.StabilityLevelDevelopment),
	)
}

// options are the same for all three signals: the component owns the
// plugin's lifecycle, and it never mutates the caller's pdata.
//
// The timeout is disabled rather than left at exporterhelper's five
// seconds, because a deadline nobody checks is worse than none — it
// reads like a runaway guest gets cut off, and no such thing happens.
// timeoutSender only derives a ctx with a deadline; honouring it would
// take building the wazero runtime with WithCloseOnContextDone(true),
// which needs a restart-or-unhealthy story for the poisoned instance
// it leaves behind. Until then, say zero and mean it.
//
// Queue and retry stay at their defaults, which is off. Turning them
// on means deciding which guest return codes are worth retrying, and
// the batch export ABI has one non-zero code covering everything from
// "the remote is down" to "this payload will never parse".
func options(component *wasm4otel.Component) []otel_helper.Option {
	return []otel_helper.Option{
		otel_helper.WithCapabilities(component.Capabilities()),
		otel_helper.WithStart(component.Start),
		otel_helper.WithShutdown(component.Shutdown),
		otel_helper.WithTimeout(otel_helper.TimeoutConfig{Timeout: 0}),
	}
}

func createLogs(
	context std_context.Context,
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
	return otel_helper.NewLogs(context, settings, anyconfig,
		component.ConsumeLogs, options(component)...)
}

func createMetrics(
	context std_context.Context,
	settings otel_exporter.Settings,
	anyconfig otel_component.Config,
) (otel_exporter.Metrics, error) {
	component, err := wasm4otel.Load(anyconfig, settings.Logger, wasm4otel.ModeExporter)
	if err != nil {
		return nil, err
	}
	if err := component.ValidateMetricsExport(); err != nil {
		return nil, err
	}
	return otel_helper.NewMetrics(context, settings, anyconfig,
		component.ConsumeMetrics, options(component)...)
}

func createTraces(
	context std_context.Context,
	settings otel_exporter.Settings,
	anyconfig otel_component.Config,
) (otel_exporter.Traces, error) {
	component, err := wasm4otel.Load(anyconfig, settings.Logger, wasm4otel.ModeExporter)
	if err != nil {
		return nil, err
	}
	if err := component.ValidateTracesExport(); err != nil {
		return nil, err
	}
	return otel_helper.NewTraces(context, settings, anyconfig,
		component.ConsumeTraces, options(component)...)
}
