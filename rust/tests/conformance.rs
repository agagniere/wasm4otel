//! The ABI v2 conformance suite, ported from `go/*/component_test.go`.
//!
//! It runs against the same hand-written WAT fixtures as the Go hosts, read
//! from `go/testdata/`, so a fixture change lands on all three hosts at once.
//! Two Go tests live next to the code they need private access to:
//! `TestGetConfigProbeThenRead` in `src/host.rs` and `TestPoisonedAfterTrap`
//! in `src/component.rs`.

use std::sync::{Arc, Mutex};
use std::time::Duration;

use tokio::sync::Notify;
use wasm4otel::{BoxError, BoxFuture, Consumer, Error, Options, Runtime, RuntimeMode, Signal};

const FULL_PLUGIN: &str = concat!(
    env!("CARGO_MANIFEST_DIR"),
    "/../go/testdata/full_plugin.wasm"
);
const PROCESSOR_ONLY_PLUGIN: &str = concat!(
    env!("CARGO_MANIFEST_DIR"),
    "/../go/testdata/processor_only_plugin.wasm"
);
const BAD_CONFIG_PLUGIN: &str = concat!(
    env!("CARGO_MANIFEST_DIR"),
    "/../go/testdata/bad_config_plugin.wasm"
);

/// What full_plugin's receive loop pushes before it starts sleeping: an OTLP
/// `LogsData` holding one empty log record.
const ONE_EMPTY_LOG: &[u8] = b"\x0a\x04\x12\x02\x12\x00";

/// The smallest `LogsData` whose one record has a body, hand-encoded so the
/// host never needs a protobuf library:
/// resource_logs { scope_logs { log_records { body { string_value: body } } } }.
fn one_log_record(body: &str) -> Vec<u8> {
    fn field(number: u8, payload: &[u8]) -> Vec<u8> {
        let mut out = vec![(number << 3) | 2, u8::try_from(payload.len()).unwrap()];
        out.extend_from_slice(payload);
        out
    }
    let any_value = field(1, body.as_bytes());
    let log_record = field(5, &any_value);
    let scope_logs = field(2, &log_record);
    let resource_logs = field(2, &scope_logs);
    field(1, &resource_logs)
}

fn runtime() -> Runtime {
    Runtime::new(RuntimeMode::Auto).expect("runtime")
}

/// A consumer that records what it is handed and wakes a waiter.
#[derive(Default)]
struct Sink {
    received: Mutex<Vec<(Signal, Vec<u8>)>>,
    notify: Notify,
}

impl Consumer for Sink {
    fn consume(&self, signal: Signal, payload: Vec<u8>) -> BoxFuture<'_, Result<(), BoxError>> {
        Box::pin(async move {
            self.received.lock().unwrap().push((signal, payload));
            self.notify.notify_one();
            Ok(())
        })
    }
}

/// The end-to-end check: the host allocates guest memory, writes the batch,
/// calls wasm4otel_process_logs, and the guest hands the bytes back through
/// push_logs. Seeing them come back out of `process` means alloc, the memory
/// write, the batch export, the push import and free all agree on the ABI.
#[tokio::test]
async fn processor_round_trip() {
    let plugin = runtime().load(FULL_PLUGIN).unwrap();
    let mut processor = plugin.processor(Options::default()).await.unwrap();
    processor.supports(Signal::Logs).unwrap();
    processor.start().await.unwrap();

    let batch = one_log_record("hello");
    let out = processor.process(Signal::Logs, &batch).await.unwrap();
    assert_eq!(out.as_deref(), Some(batch.as_slice()));

    processor.shutdown().await;
}

/// Exporter mode routes the batch to wasm4otel_export_logs rather than the
/// process_ variant: full_plugin's export_logs swallows the batch without
/// pushing, and succeeds with no consumer anywhere.
#[tokio::test]
async fn exporter_is_terminal() {
    let plugin = runtime().load(FULL_PLUGIN).unwrap();
    let mut exporter = plugin.exporter(Options::default()).await.unwrap();
    exporter.supports(Signal::Logs).unwrap();
    exporter.start().await.unwrap();

    exporter
        .export(Signal::Logs, &one_log_record("sunk"))
        .await
        .unwrap();

    exporter.shutdown().await;
}

/// The receiver lifecycle: start, then the receive loop pushes, then
/// shutdown wakes interruptible_sleep_ms so the loop unwinds and
/// `run_until` returns. The sink filling up is the evidence the loop ran;
/// `run_until` returning is the evidence shutdown reached it.
#[tokio::test]
async fn receive_loop_runs_and_stops() {
    let plugin = runtime().load(FULL_PLUGIN).unwrap();
    let sink = Arc::new(Sink::default());
    let mut receiver = plugin
        .receiver(Options::default(), sink.clone())
        .await
        .unwrap();
    receiver.start().await.unwrap();

    let stop_when_fed = {
        let sink = sink.clone();
        async move { sink.notify.notified().await }
    };
    tokio::time::timeout(Duration::from_secs(5), receiver.run_until(stop_when_fed))
        .await
        .expect("shutdown did not reach the receive loop")
        .unwrap();
    receiver.shutdown().await;

    let received = sink.received.lock().unwrap();
    assert_eq!(
        received.as_slice(),
        &[(Signal::Logs, ONE_EMPTY_LOG.to_vec())]
    );
}

/// The check instantiation runs whatever the signal: a logs processor has no
/// receive loop, so it cannot be a receiver.
#[tokio::test]
async fn mode_export_validation() {
    let plugin = runtime().load(PROCESSOR_ONLY_PLUGIN).unwrap();
    let err = plugin
        .receiver(Options::default(), Arc::new(Sink::default()))
        .await
        .err()
        .unwrap();
    assert!(
        err.to_string().contains("must export wasm4otel_receive"),
        "error = {err}"
    );
}

/// The per-signal checks. The error names the export missing for that
/// role, so the operator knows whether to fix the wiring or the plugin.
#[tokio::test]
async fn signal_export_validation() {
    let runtime = runtime();
    let processor_only = runtime.load(PROCESSOR_ONLY_PLUGIN).unwrap();
    let full = runtime.load(FULL_PLUGIN).unwrap();

    // A processor plugin asked for a signal it never declared.
    let processor = processor_only.processor(Options::default()).await.unwrap();
    let err = processor.supports(Signal::Traces).unwrap_err();
    assert!(
        err.to_string()
            .contains("does not export wasm4otel_process_traces"),
        "error = {err}"
    );

    // A processor plugin wired as an exporter.
    let exporter = processor_only.exporter(Options::default()).await.unwrap();
    let err = exporter.supports(Signal::Logs).unwrap_err();
    assert!(
        err.to_string()
            .contains("does not export wasm4otel_export_logs"),
        "error = {err}"
    );

    // A multi-role plugin asked to export a signal it only processes.
    let exporter = full.exporter(Options::default()).await.unwrap();
    let err = exporter.supports(Signal::Metrics).unwrap_err();
    assert!(
        err.to_string()
            .contains("does not export wasm4otel_export_metrics"),
        "error = {err}"
    );
}

/// A multi-role plugin is deployable in several roles at once: the checks
/// never assert that a plugin lacks another role's exports.
#[tokio::test]
async fn role_checks_are_additive() {
    let plugin = runtime().load(FULL_PLUGIN).unwrap();

    plugin
        .receiver(Options::default(), Arc::new(Sink::default()))
        .await
        .unwrap();
    let processor = plugin.processor(Options::default()).await.unwrap();
    for signal in Signal::ALL {
        processor.supports(signal).unwrap();
    }
    plugin
        .exporter(Options::default())
        .await
        .unwrap()
        .supports(Signal::Logs)
        .unwrap();
}

/// A guest returning invalid_config from wasm4otel_setup fails
/// instantiation, which is what lets a host refuse to boot on a bad
/// plugin_config rather than fail at the first batch.
#[tokio::test]
async fn setup_rejects_bad_config() {
    let plugin = runtime().load(BAD_CONFIG_PLUGIN).unwrap();
    let options = Options {
        plugin_config: Some(r#"{"threshold":"not-a-number"}"#.into()),
    };
    let err = plugin.processor(options).await.err().unwrap();
    assert!(
        err.to_string()
            .contains("rejected plugin_config as invalid"),
        "error = {err}"
    );
}

/// The path guard fires before any wasm work.
#[test]
fn missing_plugin_path() {
    assert!(matches!(runtime().load(""), Err(Error::EmptyPath)));
}

// Beyond the Go suite: what a single shared engine makes possible.

/// One runtime, one compile, three roles. Each role gets its own instance
/// and linear memory from the same compiled module, so the processor's
/// output is unaffected by the receiver running next to it.
#[tokio::test]
async fn one_compile_serves_every_role() {
    let plugin = runtime().load(FULL_PLUGIN).unwrap();
    let sink = Arc::new(Sink::default());
    let mut receiver = plugin
        .clone()
        .receiver(Options::default(), sink.clone())
        .await
        .unwrap();
    let mut processor = plugin.processor(Options::default()).await.unwrap();
    let mut exporter = plugin.exporter(Options::default()).await.unwrap();

    receiver.start().await.unwrap();
    let stop_when_fed = {
        let sink = sink.clone();
        async move { sink.notify.notified().await }
    };
    receiver.run_until(stop_when_fed).await.unwrap();

    for body in ["a", "bb", "ccc"] {
        let batch = one_log_record(body);
        assert_eq!(
            processor.process(Signal::Logs, &batch).await.unwrap(),
            Some(batch.clone())
        );
        exporter.export(Signal::Logs, &batch).await.unwrap();
    }
}

/// The interpreter backend runs the same plugin to the same result.
#[tokio::test]
async fn interpreter_runs_the_same_plugin() {
    let runtime = Runtime::new(RuntimeMode::Interpreter).unwrap();
    let mut processor = runtime
        .load(FULL_PLUGIN)
        .unwrap()
        .processor(Options::default())
        .await
        .unwrap();
    let batch = one_log_record("pulley");
    assert_eq!(
        processor.process(Signal::Logs, &batch).await.unwrap(),
        Some(batch)
    );
}

/// An empty batch never enters the guest: nothing to process is "no output",
/// nothing to export is success.
#[tokio::test]
async fn empty_batches_are_answered_by_the_host() {
    let plugin = runtime().load(FULL_PLUGIN).unwrap();
    let mut processor = plugin.processor(Options::default()).await.unwrap();
    assert_eq!(processor.process(Signal::Logs, b"").await.unwrap(), None);
    let mut exporter = plugin.exporter(Options::default()).await.unwrap();
    exporter.export(Signal::Logs, b"").await.unwrap();
}

/// Every signal round-trips through its own process_ export: the capture
/// installed for a call is keyed by that call's signal, and full_plugin's
/// process_metrics and process_traces push the matching signal back.
#[tokio::test]
async fn every_signal_round_trips() {
    let plugin = runtime().load(FULL_PLUGIN).unwrap();
    let mut processor = plugin.processor(Options::default()).await.unwrap();
    let batch = ONE_EMPTY_LOG.to_vec();
    for signal in Signal::ALL {
        assert_eq!(
            processor.process(signal, &batch).await.unwrap(),
            Some(batch.clone()),
            "{signal}"
        );
    }
}
