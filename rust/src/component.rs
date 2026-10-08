//! One plugin instance, and the three role types wrapping it.

use std::future::Future;
use std::sync::Arc;

use tokio::sync::watch;
use tracing::{error, warn};
use wasmtime::{Func, Memory, Store, TypedFunc, Val, ValType};
use wasmtime_wasi::{I32Exit, WasiCtxBuilder};

use crate::host::{Capture, Consumer, HostState};
use crate::runtime::Plugin;
use crate::{Error, Mode, Signal};

/// Per-instance settings.
#[derive(Clone, Debug, Default)]
pub struct Options {
    /// The operator's `plugin_config`, encoded as a JSON document. The guest
    /// reads it through `get_config`; `None` makes `get_config` return 0.
    pub plugin_config: Option<String>,
}

/// `wasm4otel_setup` return codes.
const SETUP_INVALID_CONFIG: u32 = 2;

type BatchFn = TypedFunc<(u32, u32), u32>;

/// The ABI exports, looked up once at instantiation. A missing export is
/// `None`, which is how the host learns what the plugin supports.
struct Exports {
    setup: Option<Func>,
    start: Option<Func>,
    receive: Option<Func>,
    shutdown: Option<Func>,
    process: [Option<BatchFn>; 3],
    export: [Option<BatchFn>; 3],
    alloc: Option<TypedFunc<u32, u32>>,
    free: Option<TypedFunc<(u32, u32), ()>>,
}

const PROCESS_NAMES: [&str; 3] = [
    "wasm4otel_process_logs",
    "wasm4otel_process_metrics",
    "wasm4otel_process_traces",
];
const EXPORT_NAMES: [&str; 3] = [
    "wasm4otel_export_logs",
    "wasm4otel_export_metrics",
    "wasm4otel_export_traces",
];

/// What the three roles share: the store, the exports, and the poison latch.
///
/// There is no mutex: every guest entry takes `&mut self`, so the borrow
/// checker gives the single-occupancy guarantee the Go hosts' `callMu`
/// enforces at run time.
pub(crate) struct Instance {
    store: Store<HostState>,
    memory: Option<Memory>,
    exports: Exports,
    mode: Mode,
    /// Latched by any trap: the instance state is undefined afterwards, so
    /// every later entry fails closed.
    broken: bool,
    shutdown: watch::Sender<bool>,
}

pub(crate) async fn instantiate(
    plugin: &Plugin,
    mode: Mode,
    options: Options,
    consumer: Option<Arc<dyn Consumer>>,
) -> Result<Instance, Error> {
    let (shutdown, shutdown_rx) = watch::channel(false);
    // wasmtime-wasi's defaults are what the Go hosts opt into: real wall and
    // monotonic clocks, and OS entropy. No stdio, no filesystem.
    let state = HostState {
        wasi: WasiCtxBuilder::new().build_p1(),
        mode,
        label: plugin.label_arc(),
        plugin_config: options
            .plugin_config
            .map(|json| Arc::from(json.into_bytes())),
        consumer,
        capture: None,
        shutdown: shutdown_rx,
    };
    let mut store = Store::new(plugin.engine(), state);
    let instance = plugin
        .pre()
        .instantiate_async(&mut store)
        .await
        .map_err(|source| Error::Instantiate { mode, source })?;

    // The WASI entry points aren't ours and keep their names. A plugin
    // declares one or the other; like wazero's `WithStartFunctions`, run
    // whichever exists, `_start` first.
    for name in ["_start", "_initialize"] {
        let Some(func) = instance.get_func(&mut store, name) else {
            continue;
        };
        let entry = func
            .typed::<(), ()>(&store)
            .map_err(|source| Error::Instantiate { mode, source })?;
        if let Err(source) = entry.call_async(&mut store, ()).await {
            // A WASI command ends `_start` with proc_exit; 0 is success.
            if source
                .downcast_ref::<I32Exit>()
                .is_some_and(|exit| exit.0 == 0)
            {
                continue;
            }
            return Err(Error::Instantiate { mode, source });
        }
    }

    let mut lookup = |name: &'static str| instance.get_func(&mut store, name);
    let raw = (
        lookup("wasm4otel_setup"),
        lookup("wasm4otel_start"),
        lookup("wasm4otel_receive"),
        lookup("wasm4otel_shutdown"),
        PROCESS_NAMES.map(&mut lookup),
        EXPORT_NAMES.map(&mut lookup),
        lookup("wasm4otel_alloc"),
        lookup("wasm4otel_free"),
    );
    let memory = instance.get_memory(&mut store, "memory");

    let lifecycle = |func: Option<Func>, export| check_lifecycle(&store, mode, func, export);
    let typed_batch = |funcs: [Option<Func>; 3],
                       names: [&'static str; 3]|
     -> Result<[Option<BatchFn>; 3], Error> {
        let [a, b, c] = funcs;
        Ok([
            typed(&store, mode, a, names[0])?,
            typed(&store, mode, b, names[1])?,
            typed(&store, mode, c, names[2])?,
        ])
    };
    let exports = Exports {
        setup: lifecycle(raw.0, "wasm4otel_setup")?,
        start: lifecycle(raw.1, "wasm4otel_start")?,
        receive: lifecycle(raw.2, "wasm4otel_receive")?,
        shutdown: lifecycle(raw.3, "wasm4otel_shutdown")?,
        process: typed_batch(raw.4, PROCESS_NAMES)?,
        export: typed_batch(raw.5, EXPORT_NAMES)?,
        alloc: typed(&store, mode, raw.6, "wasm4otel_alloc")?,
        free: typed(&store, mode, raw.7, "wasm4otel_free")?,
    };

    let mut instance = Instance {
        store,
        memory,
        exports,
        mode,
        broken: false,
        shutdown,
    };
    instance.validate_mode_exports()?;
    instance.run_setup().await?;
    Ok(instance)
}

/// Lifecycle exports are `() -> i32`, or `() -> ()` for forward
/// compatibility (read as success).
fn check_lifecycle(
    store: &Store<HostState>,
    mode: Mode,
    func: Option<Func>,
    export: &'static str,
) -> Result<Option<Func>, Error> {
    let Some(func) = func else { return Ok(None) };
    let ty = func.ty(store);
    let results: Vec<_> = ty.results().collect();
    let ok = ty.params().len() == 0
        && (results.is_empty() || (results.len() == 1 && matches!(results[0], ValType::I32)));
    if ok {
        Ok(Some(func))
    } else {
        Err(Error::Signature { mode, export })
    }
}

fn typed<P, R>(
    store: &Store<HostState>,
    mode: Mode,
    func: Option<Func>,
    export: &'static str,
) -> Result<Option<TypedFunc<P, R>>, Error>
where
    P: wasmtime::WasmParams,
    R: wasmtime::WasmResults,
{
    func.map(|f| {
        f.typed(store)
            .map_err(|_| Error::Signature { mode, export })
    })
    .transpose()
}

impl Instance {
    /// The exports a role needs whatever signal it is wired for. Checks are
    /// additive: exporting more than this role uses is fine.
    fn validate_mode_exports(&self) -> Result<(), Error> {
        let mode = self.mode;
        match mode {
            Mode::Receiver if self.exports.receive.is_none() => Err(Error::ModeExport {
                mode,
                what: "wasm4otel_receive",
            }),
            Mode::Processor | Mode::Exporter
                if self.exports.alloc.is_none() || self.exports.free.is_none() =>
            {
                Err(Error::ModeExport {
                    mode,
                    what: "wasm4otel_alloc and wasm4otel_free",
                })
            }
            _ => Ok(()),
        }
    }

    /// The guest's chance to read `plugin_config` and refuse it.
    async fn run_setup(&mut self) -> Result<(), Error> {
        let Some(setup) = self.exports.setup else {
            return Ok(());
        };
        let mode = self.mode;
        match self.call_status(setup, "wasm4otel_setup").await? {
            None | Some(0) => Ok(()),
            Some(SETUP_INVALID_CONFIG) => Err(Error::InvalidConfig { mode }),
            Some(code) => Err(Error::SetupFailed { mode, code }),
        }
    }

    /// Short-lived init, the same in every role.
    async fn start(&mut self) -> Result<(), Error> {
        let Some(start) = self.exports.start else {
            return Ok(());
        };
        self.check_start_status(start, "wasm4otel_start").await
    }

    async fn check_start_status(&mut self, func: Func, export: &'static str) -> Result<(), Error> {
        match self.call_status(func, export).await? {
            None | Some(0) => Ok(()),
            Some(code) => Err(Error::StartFailed {
                mode: self.mode,
                export,
                code,
            }),
        }
    }

    /// Signals shutdown, then runs `wasm4otel_shutdown`. Errors are logged:
    /// there is nothing a caller could do about them.
    async fn shutdown(&mut self) {
        self.shutdown.send_replace(true);
        let Some(shutdown) = self.exports.shutdown else {
            return;
        };
        if let Err(e) = self.call_status(shutdown, "wasm4otel_shutdown").await {
            warn!(plugin = %self.store.data().label, error = %e, "shutdown returned with error");
        }
    }

    /// Calls a lifecycle export, returning its status code if it has one.
    async fn call_status(
        &mut self,
        func: Func,
        export: &'static str,
    ) -> Result<Option<u32>, Error> {
        if self.broken {
            return Err(Error::Poisoned);
        }
        let mut results = vec![Val::I32(0); func.ty(&self.store).results().len()];
        if let Err(source) = func.call_async(&mut self.store, &[], &mut results).await {
            return Err(self.poison(export, source));
        }
        Ok(results.first().and_then(Val::i32).map(|rc| rc as u32))
    }

    fn poison(&mut self, export: &'static str, source: wasmtime::Error) -> Error {
        self.broken = true;
        let error = Error::Trap {
            mode: self.mode,
            export,
            source,
        };
        warn!(plugin = %self.store.data().label, error = %error, "guest trapped");
        error
    }

    /// The export serving `signal` in this instance's role.
    fn batch_export(&self, signal: Signal) -> Result<(BatchFn, &'static str), Error> {
        let (funcs, names) = match self.mode {
            Mode::Exporter => (&self.exports.export, EXPORT_NAMES),
            _ => (&self.exports.process, PROCESS_NAMES),
        };
        let name = names[signal.index()];
        funcs[signal.index()]
            .clone()
            .map(|f| (f, name))
            .ok_or(Error::SignalExport {
                mode: self.mode,
                export: name,
                signal,
            })
    }

    /// Runs alloc → write → batch export → free, and turns the guest's
    /// return code into an error. The caller has rejected empty payloads.
    async fn deliver(&mut self, signal: Signal, payload: &[u8]) -> Result<(), Error> {
        let (func, export) = self.batch_export(signal)?;
        if self.broken {
            return Err(Error::Poisoned);
        }
        let size =
            u32::try_from(payload.len()).map_err(|_| Error::PayloadTooLarge(payload.len()))?;
        // validate_mode_exports guarantees both for processors and exporters.
        let (Some(alloc), Some(free)) = (self.exports.alloc.clone(), self.exports.free.clone())
        else {
            unreachable!("processor and exporter instances always have alloc and free");
        };

        let ptr = match alloc.call_async(&mut self.store, size).await {
            Ok(ptr) => ptr,
            Err(source) => return Err(self.poison("wasm4otel_alloc", source)),
        };
        if ptr == 0 {
            // The guest is out of memory; there is no buffer to free.
            return Err(Error::GuestAlloc(size));
        }

        let written = self
            .memory
            .is_some_and(|memory| memory.write(&mut self.store, ptr as usize, payload).is_ok());
        if !written {
            if let Err(source) = free.call_async(&mut self.store, (ptr, size)).await {
                self.poison("wasm4otel_free", source);
            }
            return Err(Error::OutOfRange { ptr, size });
        }

        let rc = match func.call_async(&mut self.store, (ptr, size)).await {
            Ok(rc) => rc,
            // Don't free: the instance state is undefined after a trap.
            Err(source) => return Err(self.poison(export, source)),
        };
        if let Err(source) = free.call_async(&mut self.store, (ptr, size)).await {
            return Err(self.poison("wasm4otel_free", source));
        }
        if rc != 0 {
            warn!(plugin = %self.store.data().label, rc, bytes = size, "{export} returned non-zero");
            return Err(Error::GuestReturned { export, code: rc });
        }
        Ok(())
    }
}

/// A plugin instance in the receiver role.
///
/// Drive it in order: [`start`](Receiver::start),
/// [`run_until`](Receiver::run_until), [`shutdown`](Receiver::shutdown).
pub struct Receiver {
    inner: Instance,
}

impl Receiver {
    pub(crate) fn new(inner: Instance) -> Self {
        Self { inner }
    }

    /// Runs `wasm4otel_start`, which must return promptly.
    ///
    /// # Errors
    ///
    /// Fails when the guest traps or returns a non-zero code.
    pub async fn start(&mut self) -> Result<(), Error> {
        self.inner.start().await
    }

    /// Runs `wasm4otel_receive` until it returns.
    ///
    /// When `shutdown` resolves, `interruptible_sleep_ms` starts returning
    /// "interrupted" so the guest's loop can unwind, and this keeps waiting
    /// for it to do so. Drive the returned future to completion: dropping it
    /// mid-call abandons the guest in an undefined state.
    ///
    /// # Errors
    ///
    /// Fails when the loop traps or returns a non-zero code.
    pub async fn run_until(&mut self, shutdown: impl Future<Output = ()>) -> Result<(), Error> {
        let inner = &mut self.inner;
        let receive = inner.exports.receive.expect("validated at instantiation");
        let stop = inner.shutdown.clone();
        let result = {
            let run = inner.check_start_status(receive, "wasm4otel_receive");
            tokio::pin!(run, shutdown);
            tokio::select! {
                result = &mut run => result,
                () = &mut shutdown => {
                    stop.send_replace(true);
                    run.await
                }
            }
        };
        if let Err(e) = &result {
            error!(plugin = %inner.store.data().label, error = %e, "receive loop stopped");
        }
        result
    }

    /// Signals shutdown and runs `wasm4otel_shutdown`.
    pub async fn shutdown(&mut self) {
        self.inner.shutdown().await;
    }
}

/// A plugin instance in the processor role.
pub struct Processor {
    inner: Instance,
}

impl Processor {
    pub(crate) fn new(inner: Instance) -> Self {
        Self { inner }
    }

    /// Checks that the plugin exports `wasm4otel_process_<signal>`.
    ///
    /// # Errors
    ///
    /// Names the missing export, so the operator knows whether to fix the
    /// wiring or rebuild the plugin.
    pub fn supports(&self, signal: Signal) -> Result<(), Error> {
        self.inner.batch_export(signal).map(drop)
    }

    /// Runs `wasm4otel_start`, which must return promptly.
    ///
    /// # Errors
    ///
    /// Fails when the guest traps or returns a non-zero code.
    pub async fn start(&mut self) -> Result<(), Error> {
        self.inner.start().await
    }

    /// Hands one batch to `wasm4otel_process_<signal>` and returns what the
    /// guest pushed back during the call, concatenated into one payload.
    ///
    /// `None` means the guest pushed nothing: it filtered the whole batch
    /// out. An empty input is answered `None` without entering the guest.
    ///
    /// # Errors
    ///
    /// Fails when the plugin does not serve `signal`, when the guest traps
    /// or returns a non-zero code, or when the instance is poisoned.
    pub async fn process(
        &mut self,
        signal: Signal,
        payload: &[u8],
    ) -> Result<Option<Vec<u8>>, Error> {
        if payload.is_empty() {
            return Ok(None);
        }
        let inner = &mut self.inner;
        inner.store.data_mut().capture = Some(Capture {
            signal,
            bytes: Vec::new(),
        });
        let delivered = inner.deliver(signal, payload).await;
        // Taken on every path, so the capture never outlives its call.
        let captured = inner.store.data_mut().capture.take().map(|c| c.bytes);
        delivered?;
        Ok(captured.filter(|bytes| !bytes.is_empty()))
    }

    /// Signals shutdown and runs `wasm4otel_shutdown`.
    pub async fn shutdown(&mut self) {
        self.inner.shutdown().await;
    }
}

/// A plugin instance in the exporter role.
pub struct Exporter {
    inner: Instance,
}

impl Exporter {
    pub(crate) fn new(inner: Instance) -> Self {
        Self { inner }
    }

    /// Checks that the plugin exports `wasm4otel_export_<signal>`.
    ///
    /// # Errors
    ///
    /// Names the missing export, so the operator knows whether to fix the
    /// wiring or rebuild the plugin.
    pub fn supports(&self, signal: Signal) -> Result<(), Error> {
        self.inner.batch_export(signal).map(drop)
    }

    /// Runs `wasm4otel_start`, which must return promptly.
    ///
    /// # Errors
    ///
    /// Fails when the guest traps or returns a non-zero code.
    pub async fn start(&mut self) -> Result<(), Error> {
        self.inner.start().await
    }

    /// Hands one batch to the terminal `wasm4otel_export_<signal>`. An empty
    /// batch succeeds without entering the guest.
    ///
    /// # Errors
    ///
    /// Fails when the plugin does not serve `signal`, when the guest traps
    /// or returns a non-zero code, or when the instance is poisoned.
    pub async fn export(&mut self, signal: Signal, payload: &[u8]) -> Result<(), Error> {
        if payload.is_empty() {
            return Ok(());
        }
        self.inner.deliver(signal, payload).await
    }

    /// Signals shutdown and runs `wasm4otel_shutdown`.
    pub async fn shutdown(&mut self) {
        self.inner.shutdown().await;
    }
}

#[cfg(test)]
mod tests {
    use crate::{Options, Runtime, RuntimeMode, Signal};

    const FULL_PLUGIN: &str = concat!(
        env!("CARGO_MANIFEST_DIR"),
        "/../go/testdata/full_plugin.wasm"
    );

    /// Once a guest call has trapped, further entries are refused rather
    /// than compounding the corruption. None of the fixtures trap on
    /// demand, so the latch is set by hand to test the guard itself.
    #[tokio::test]
    async fn poisoned_after_trap() {
        let runtime = Runtime::new(RuntimeMode::Auto).unwrap();
        let plugin = runtime.load(FULL_PLUGIN).unwrap();
        let mut exporter = plugin.exporter(Options::default()).await.unwrap();
        exporter.inner.broken = true;

        let err = exporter
            .export(Signal::Logs, b"\x0a\x00")
            .await
            .unwrap_err();
        assert!(err.to_string().contains("poisoned"), "error = {err}");
    }
}
