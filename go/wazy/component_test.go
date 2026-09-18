package wasm4otel

import (
	std_context "context"
	std_strings "strings"
	std_testing "testing"
	std_time "time"

	uber_zap "go.uber.org/zap"

	otel_consumertest "go.opentelemetry.io/collector/consumer/consumertest"
	otel_logs "go.opentelemetry.io/collector/pdata/plog"
)

const (
	fullPlugin          = "testdata/full_plugin.wasm"
	processorOnlyPlugin = "testdata/processor_only_plugin.wasm"
	badConfigPlugin     = "testdata/bad_config_plugin.wasm"
)

func testConfig(path string) Config {
	return Config{Path: path}
}

// oneLogRecord builds the smallest batch that survives a marshal /
// unmarshal round-trip, so a fixture forwarding it verbatim is
// observable downstream.
func oneLogRecord(body string) otel_logs.Logs {
	logs := otel_logs.NewLogs()
	record := logs.ResourceLogs().AppendEmpty().
		ScopeLogs().AppendEmpty().
		LogRecords().AppendEmpty()
	record.Body().SetStr(body)
	return logs
}

// TestProcessorRoundTrip is the end-to-end check: the host marshals a
// batch, allocates guest memory, calls wasm4otel_process_logs, and the
// guest hands the bytes back through push_logs. Seeing the record come
// out of the downstream sink means alloc, memory write, the batch
// export, the push import and free all agree on the ABI.
func TestProcessorRoundTrip(t *std_testing.T) {
	component, err := Load(testConfig(fullPlugin), uber_zap.NewNop(), ModeProcessor)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if err := component.ValidateLogsExport(); err != nil {
		t.Fatalf("ValidateLogsExport: %v", err)
	}
	sink := new(otel_consumertest.LogsSink)
	component.NextConsumerLogs = sink

	if err := component.Start(std_context.Background(), nil); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer component.Shutdown(std_context.Background())

	if err := component.ConsumeLogs(std_context.Background(), oneLogRecord("hello")); err != nil {
		t.Fatalf("ConsumeLogs: %v", err)
	}

	if got := sink.LogRecordCount(); got != 1 {
		t.Fatalf("downstream record count = %d, want 1", got)
	}
	body := sink.AllLogs()[0].ResourceLogs().At(0).
		ScopeLogs().At(0).LogRecords().At(0).Body().Str()
	if body != "hello" {
		t.Errorf("forwarded body = %q, want %q", body, "hello")
	}
}

// TestExporterIsTerminal checks that exporter mode routes the batch to
// wasm4otel_export_logs rather than the process_ variant, and tolerates
// having no downstream consumer wired. full_plugin's export_logs
// swallows the batch without calling push_logs, so a nil
// NextConsumerLogs is not an error here — which is exactly the
// distinction between the two modes.
func TestExporterIsTerminal(t *std_testing.T) {
	component, err := Load(testConfig(fullPlugin), uber_zap.NewNop(), ModeExporter)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if err := component.ValidateLogsExport(); err != nil {
		t.Fatalf("ValidateLogsExport: %v", err)
	}
	if err := component.Start(std_context.Background(), nil); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer component.Shutdown(std_context.Background())

	if err := component.ConsumeLogs(std_context.Background(), oneLogRecord("sunk")); err != nil {
		t.Fatalf("ConsumeLogs: %v", err)
	}
}

// TestReceiveLoopRunsAndStops covers the receiver lifecycle: Start runs
// wasm4otel_start and then spawns wasm4otel_receive on its own
// goroutine, and Shutdown cancels the context so interruptible_sleep_ms
// wakes the loop and lets it unwind before wasm4otel_shutdown runs.
//
// The fixture's loop pushes one batch before it starts sleeping, so the
// sink filling up is the evidence the goroutine really ran; Shutdown
// returning is the evidence receiveWg joined it.
func TestReceiveLoopRunsAndStops(t *std_testing.T) {
	component, err := Load(testConfig(fullPlugin), uber_zap.NewNop(), ModeReceiver)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	sink := new(otel_consumertest.LogsSink)
	component.NextConsumerLogs = sink

	if err := component.Start(std_context.Background(), nil); err != nil {
		t.Fatalf("Start: %v", err)
	}

	deadline := std_time.Now().Add(5 * std_time.Second)
	for sink.LogRecordCount() == 0 && std_time.Now().Before(deadline) {
		std_time.Sleep(5 * std_time.Millisecond)
	}
	if got := sink.LogRecordCount(); got == 0 {
		t.Error("receive loop pushed nothing; it likely never ran")
	}

	done := make(chan error, 1)
	go func() { done <- component.Shutdown(std_context.Background()) }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Shutdown: %v", err)
		}
	case <-std_time.After(5 * std_time.Second):
		t.Fatal("Shutdown did not join the receive loop")
	}
}

// TestModeExportValidation covers the checks Load itself runs: the ones
// a role needs whatever signal it was wired for.
func TestModeExportValidation(t *std_testing.T) {
	// A logs processor has no receive loop, so it cannot be wired under
	// `receivers:`.
	_, err := Load(testConfig(processorOnlyPlugin), uber_zap.NewNop(), ModeReceiver)
	if err == nil {
		t.Fatal("Load succeeded for a processor-only plugin in receiver mode")
	}
	if !std_strings.Contains(err.Error(), "must export wasm4otel_receive") {
		t.Errorf("Load error = %q, want it to name the missing receive export", err)
	}
}

// TestSignalExportValidation covers the per-signal checks the factories
// run after Load. The error names the export missing for *that* role,
// so the operator knows whether to fix the YAML or rebuild the plugin.
func TestSignalExportValidation(t *std_testing.T) {
	for _, testcase := range []struct {
		name     string
		path     string
		mode     ComponentMode
		validate func(*Component) error
		wantErr  string
	}{{
		name:     "processor plugin asked for a signal it never declared",
		path:     processorOnlyPlugin,
		mode:     ModeProcessor,
		validate: (*Component).ValidateTracesExport,
		wantErr:  "does not export wasm4otel_process_traces",
	}, {
		name:     "processor plugin wired as an exporter",
		path:     processorOnlyPlugin,
		mode:     ModeExporter,
		validate: (*Component).ValidateLogsExport,
		wantErr:  "does not export wasm4otel_export_logs",
	}, {
		name:     "multi-role plugin asked to export a signal it only processes",
		path:     fullPlugin,
		mode:     ModeExporter,
		validate: (*Component).ValidateMetricsExport,
		wantErr:  "does not export wasm4otel_export_metrics",
	}} {
		t.Run(testcase.name, func(t *std_testing.T) {
			component, err := Load(testConfig(testcase.path), uber_zap.NewNop(), testcase.mode)
			if err != nil {
				t.Fatalf("Load: %v", err)
			}
			defer component.Shutdown(std_context.Background())

			err = testcase.validate(component)
			if err == nil {
				t.Fatalf("validation passed, want an error containing %q", testcase.wantErr)
			}
			if !std_strings.Contains(err.Error(), testcase.wantErr) {
				t.Errorf("validation error = %q, want it to contain %q", err, testcase.wantErr)
			}
		})
	}
}

// TestRoleChecksAreAdditive pins the rule that a multi-role plugin is
// deployable in several YAML sections at once: the checks never assert
// that a plugin lacks another role's exports. full_plugin exports a
// receive loop, three process_ variants and export_logs, and every one
// of those wirings has to be accepted.
func TestRoleChecksAreAdditive(t *std_testing.T) {
	for _, testcase := range []struct {
		name     string
		mode     ComponentMode
		validate func(*Component) error
	}{
		{"receiver", ModeReceiver, nil},
		{"logs processor", ModeProcessor, (*Component).ValidateLogsExport},
		{"metrics processor", ModeProcessor, (*Component).ValidateMetricsExport},
		{"traces processor", ModeProcessor, (*Component).ValidateTracesExport},
		{"logs exporter", ModeExporter, (*Component).ValidateLogsExport},
	} {
		t.Run(testcase.name, func(t *std_testing.T) {
			component, err := Load(testConfig(fullPlugin), uber_zap.NewNop(), testcase.mode)
			if err != nil {
				t.Fatalf("Load: %v", err)
			}
			defer component.Shutdown(std_context.Background())

			if testcase.validate != nil {
				if err := testcase.validate(component); err != nil {
					t.Errorf("validation rejected a plugin that exports this role: %v", err)
				}
			}
		})
	}
}

// TestSetupRejectsBadConfig checks the v2 config-validation hook: a
// guest returning invalid_config from wasm4otel_setup fails Load, which
// is what makes the collector refuse to boot on a bad plugin_config
// rather than failing at the first batch.
func TestSetupRejectsBadConfig(t *std_testing.T) {
	config := Config{
		Path:         badConfigPlugin,
		PluginConfig: PluginConfig{"threshold": "not-a-number"},
	}
	_, err := Load(config, uber_zap.NewNop(), ModeProcessor)
	if err == nil {
		t.Fatal("Load succeeded, want an invalid-config error")
	}
	if !std_strings.Contains(err.Error(), "rejected plugin_config as invalid") {
		t.Errorf("Load error = %q, want it to mention an invalid plugin_config", err)
	}
}

// TestMissingPluginPath checks the config guard fires before any wasm
// work is attempted.
func TestMissingPluginPath(t *std_testing.T) {
	if _, err := Load(Config{}, uber_zap.NewNop(), ModeProcessor); err == nil {
		t.Fatal("Load succeeded with an empty path, want an error")
	}
}

// TestGetConfigProbeThenRead exercises the two-step get_config
// contract directly: a (0, 0) probe reports the document size without
// touching guest memory, an undersized buffer still reports the true
// size and writes nothing, and a large enough buffer gets the bytes.
func TestGetConfigProbeThenRead(t *std_testing.T) {
	component, err := Load(
		Config{Path: fullPlugin, PluginConfig: PluginConfig{"level": "warn"}},
		uber_zap.NewNop(), ModeProcessor)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	defer component.Shutdown(std_context.Background())

	wantJSON := `{"level":"warn"}`
	size := component.getConfig(std_context.Background(), component.instance, 0, 0)
	if int(size) != len(wantJSON) {
		t.Fatalf("probe size = %d, want %d", size, len(wantJSON))
	}

	// Undersized buffer: nothing written, true size still reported.
	scratch := component.instance.ExportedFunction("wasm4otel_alloc")
	allocated, err := scratch.Call(std_context.Background(), uint64(size))
	if err != nil {
		t.Fatalf("alloc: %v", err)
	}
	ptr := uint32(allocated[0])
	if got := component.getConfig(std_context.Background(), component.instance, ptr, size-1); got != size {
		t.Errorf("undersized call returned %d, want the true size %d", got, size)
	}
	if buffer, ok := component.instance.Memory().Read(ptr, size); !ok {
		t.Fatal("unable to read back the scratch buffer")
	} else if std_strings.Contains(string(buffer), "warn") {
		t.Error("undersized call wrote a truncated document into guest memory")
	}

	// Large enough buffer: bytes land in guest memory.
	if got := component.getConfig(std_context.Background(), component.instance, ptr, size); got != size {
		t.Fatalf("sized call returned %d, want %d", got, size)
	}
	buffer, ok := component.instance.Memory().Read(ptr, size)
	if !ok {
		t.Fatal("unable to read back the config buffer")
	}
	if string(buffer) != wantJSON {
		t.Errorf("guest sees config %q, want %q", buffer, wantJSON)
	}
}

// TestPoisonedAfterTrap checks the fail-closed behaviour: once a guest
// call has trapped, further entries are refused rather than compounding
// the corruption. None of the fixtures trap on demand, so we latch
// `broken` by hand to test the guard itself.
func TestPoisonedAfterTrap(t *std_testing.T) {
	component, err := Load(testConfig(fullPlugin), uber_zap.NewNop(), ModeProcessor)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	defer component.Shutdown(std_context.Background())
	component.NextConsumerLogs = new(otel_consumertest.LogsSink)

	component.callMu.Lock()
	component.broken = true
	component.callMu.Unlock()

	err = component.ConsumeLogs(std_context.Background(), oneLogRecord("after trap"))
	if err == nil {
		t.Fatal("ConsumeLogs succeeded on a poisoned component, want an error")
	}
	if !std_strings.Contains(err.Error(), "poisoned") {
		t.Errorf("error = %q, want it to mention the component is poisoned", err)
	}
}
