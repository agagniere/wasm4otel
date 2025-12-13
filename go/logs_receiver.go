package wasm4otel

import (
	std_context "context"
	std_time "time"

	uber_zap "go.uber.org/zap"

	otel_component "go.opentelemetry.io/collector/component"
	otel_consumer "go.opentelemetry.io/collector/consumer"
	otel_data "go.opentelemetry.io/collector/pdata/pcommon"
	otel_log "go.opentelemetry.io/collector/pdata/plog"
)

type WasmOtelLogsReceiver struct {
	logger  *uber_zap.SugaredLogger
	sink    otel_consumer.Logs
	context std_context.Context
	cancel  std_context.CancelFunc
}

func (self *WasmOtelLogsReceiver) Start(context std_context.Context, host otel_component.Host) error {
	go self.generateLogs()
	return nil
}

func (self *WasmOtelLogsReceiver) Shutdown(ctx std_context.Context) error {
	self.cancel()
	return nil
}

func (self *WasmOtelLogsReceiver) generateLogs() {
	ticker := std_time.NewTicker(5 * std_time.Second)

	for {
		select {
		case <-self.context.Done():
			break
		case <-ticker.C:
			{
				log_collection := otel_log.NewLogs()

				logs_from_host := log_collection.ResourceLogs().AppendEmpty()

				logs_from_host.Resource().Attributes().PutStr("service.name", "totofoo")
				logs_from_host.Resource().Attributes().PutStr("service.version", "0.0.1")
				logs_from_host.Resource().Attributes().PutStr("host.name", "agagniere-ddmacbook")
				logs_from_host.Resource().Attributes().PutStr("server.address", "gagniere.dev")
				logs_from_host.Resource().Attributes().PutStr("os.type", "darwin")
				logs_from_host.Resource().Attributes().PutStr("os.version", "25.1.0")
				logs_from_host.Resource().Attributes().PutStr("k8s.cluster.name", "agagniere")

				logs_from_instance := logs_from_host.ScopeLogs().AppendEmpty()
				logs_from_instance.Scope().SetName("wasm4otel_log_generator")
				logs_from_instance.Scope().SetVersion("0.0.1")
				logs_from_instance.Scope().Attributes().PutStr("otel.component.name", "wasm4otel/logsreceiver")
				logs_from_instance.Scope().Attributes().PutStr("otel.component.type", "wasm4otel")
				logs_from_instance.Scope().Attributes().PutStr("process.executable.name", "otelcontribcol")

				log := logs_from_instance.LogRecords().AppendEmpty()
				log.SetTimestamp(otel_data.Timestamp(std_time.Now().UnixNano()))
				log.SetSeverityNumber(otel_log.SeverityNumberInfo)
				log.SetSeverityText("INFO")
				log.Body().SetStr("Hello from a custom receiver")

				self.sink.ConsumeLogs(self.context, log_collection)
			}
		}
	}
}
