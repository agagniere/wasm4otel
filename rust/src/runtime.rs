//! The process-wide engine, and plugins compiled against it.

use std::fmt;
use std::path::Path;
use std::str::FromStr;
use std::sync::Arc;

use wasmtime::{Config, Engine, InstancePre, Linker, Module};

use crate::Error;
use crate::component::{self, Exporter, Options, Processor, Receiver};
use crate::host::{self, Consumer, HostState};

/// Which execution backend the engine uses.
///
/// Same values, and same default, as the Go hosts' `runtime.mode`. It is
/// engine-wide here: one [`Runtime`] has one engine, so every plugin it
/// loads runs on the same backend.
#[derive(Clone, Copy, Debug, Default, Eq, PartialEq)]
pub enum RuntimeMode {
    /// Native code where Cranelift has a backend for this CPU (x86_64,
    /// aarch64, s390x, riscv64), the Pulley interpreter elsewhere.
    #[default]
    Auto,
    /// The Pulley interpreter, everywhere. Needs no executable memory, and
    /// is much slower.
    Interpreter,
    /// Native code, or an error when this CPU has no Cranelift backend.
    Compiled,
}

impl RuntimeMode {
    const fn as_str(self) -> &'static str {
        match self {
            RuntimeMode::Auto => "auto",
            RuntimeMode::Interpreter => "interpreter",
            RuntimeMode::Compiled => "compiled",
        }
    }
}

impl fmt::Display for RuntimeMode {
    fn fmt(&self, f: &mut fmt::Formatter<'_>) -> fmt::Result {
        f.write_str(self.as_str())
    }
}

impl FromStr for RuntimeMode {
    type Err = Error;

    fn from_str(s: &str) -> Result<Self, Self::Err> {
        match s {
            "" | "auto" => Ok(RuntimeMode::Auto),
            "interpreter" => Ok(RuntimeMode::Interpreter),
            "compiled" => Ok(RuntimeMode::Compiled),
            other => Err(Error::Runtime(format!(
                "unknown runtime mode {other:?}; expected auto, interpreter or compiled"
            ))),
        }
    }
}

/// Whether Cranelift can generate native code for the CPU we run on.
const HAS_NATIVE_BACKEND: bool = cfg!(any(
    target_arch = "x86_64",
    target_arch = "aarch64",
    target_arch = "s390x",
    target_arch = "riscv64"
));

/// The Pulley target matching this host's pointer width and endianness.
fn pulley_target() -> &'static str {
    match (
        cfg!(target_pointer_width = "64"),
        cfg!(target_endian = "little"),
    ) {
        (true, true) => "pulley64",
        (true, false) => "pulley64be",
        (false, true) => "pulley32",
        (false, false) => "pulley32be",
    }
}

/// One wasmtime engine and one import linker, shared by every plugin.
///
/// Create one per process and clone it freely: clones share the engine. The
/// linker defines the `env` imports and WASIp1 once; each instance's state
/// lives in its own store, so nothing in the linker is per-plugin.
#[derive(Clone)]
pub struct Runtime {
    engine: Engine,
    linker: Arc<Linker<HostState>>,
    mode: RuntimeMode,
}

impl Runtime {
    /// Creates the engine for `mode` and defines the host imports.
    ///
    /// # Errors
    ///
    /// Fails when `mode` is [`RuntimeMode::Compiled`] on a CPU without a
    /// Cranelift backend, or when wasmtime rejects the configuration.
    pub fn new(mode: RuntimeMode) -> Result<Self, Error> {
        let mut config = Config::new();
        match mode {
            RuntimeMode::Auto => {}
            RuntimeMode::Compiled if !HAS_NATIVE_BACKEND => {
                return Err(Error::Runtime(format!(
                    "runtime mode compiled is not available on {}; use auto or interpreter",
                    std::env::consts::ARCH
                )));
            }
            RuntimeMode::Compiled => {}
            RuntimeMode::Interpreter => {
                config
                    .target(pulley_target())
                    .map_err(|e| Error::Runtime(format!("{e:#}")))?;
            }
        }
        let engine = Engine::new(&config).map_err(|e| Error::Runtime(format!("{e:#}")))?;

        let mut linker = Linker::new(&engine);
        wasmtime_wasi::p1::add_to_linker_async(&mut linker, |state: &mut HostState| {
            &mut state.wasi
        })
        .and_then(|()| host::add_to_linker(&mut linker))
        .map_err(|e| Error::Runtime(format!("unable to define host imports: {e:#}")))?;

        Ok(Self {
            engine,
            linker: Arc::new(linker),
            mode,
        })
    }

    /// The backend this runtime was created with.
    pub fn mode(&self) -> RuntimeMode {
        self.mode
    }

    /// Reads and compiles the plugin at `path`.
    ///
    /// # Errors
    ///
    /// Fails when the path is empty or unreadable, when the file is not a
    /// valid module, or when it imports something this host does not
    /// provide.
    pub fn load(&self, path: impl AsRef<Path>) -> Result<Plugin, Error> {
        let path = path.as_ref();
        if path.as_os_str().is_empty() {
            return Err(Error::EmptyPath);
        }
        let bytes = std::fs::read(path).map_err(|source| Error::Read {
            path: path.to_path_buf(),
            source,
        })?;
        self.compile(path.display().to_string(), &bytes)
    }

    /// Compiles a plugin from its bytes. `label` tags the plugin's logs and
    /// errors; [`Runtime::load`] uses the path.
    ///
    /// # Errors
    ///
    /// Fails when the bytes are not a valid module, or when the module
    /// imports something this host does not provide.
    pub fn compile(&self, label: impl Into<String>, bytes: &[u8]) -> Result<Plugin, Error> {
        let label = label.into();
        // Resolving the imports here, once, means an unknown import fails at
        // load time and every later instantiation skips the lookup.
        let pre = Module::new(&self.engine, bytes)
            .and_then(|module| self.linker.instantiate_pre(&module))
            .map_err(|source| Error::Compile {
                plugin: label.clone(),
                source,
            })?;
        Ok(Plugin {
            engine: self.engine.clone(),
            pre,
            label: label.into(),
        })
    }
}

/// A compiled plugin, ready to be instantiated in any role.
///
/// Cloning is cheap and shares the compiled code. Each role method creates
/// a new instance with its own memory, so one `.wasm` can serve as a
/// receiver, a processor and an exporter at once, compiled once.
#[derive(Clone)]
pub struct Plugin {
    engine: Engine,
    pre: InstancePre<HostState>,
    label: Arc<str>,
}

impl Plugin {
    /// The label this plugin's logs and errors are tagged with.
    pub fn label(&self) -> &str {
        &self.label
    }

    /// Instantiates the plugin as a receiver forwarding to `consumer`, then
    /// runs `wasm4otel_setup`.
    ///
    /// # Errors
    ///
    /// Fails when the plugin does not export `wasm4otel_receive`, when
    /// instantiation traps, or when setup reports failure.
    pub async fn receiver(
        &self,
        options: Options,
        consumer: Arc<dyn Consumer>,
    ) -> Result<Receiver, Error> {
        component::instantiate(self, crate::Mode::Receiver, options, Some(consumer))
            .await
            .map(Receiver::new)
    }

    /// Instantiates the plugin as a processor, then runs `wasm4otel_setup`.
    ///
    /// # Errors
    ///
    /// Fails when the plugin does not export `wasm4otel_alloc` and
    /// `wasm4otel_free`, when instantiation traps, or when setup reports
    /// failure.
    pub async fn processor(&self, options: Options) -> Result<Processor, Error> {
        component::instantiate(self, crate::Mode::Processor, options, None)
            .await
            .map(Processor::new)
    }

    /// Instantiates the plugin as an exporter, then runs `wasm4otel_setup`.
    ///
    /// # Errors
    ///
    /// Same as [`Plugin::processor`].
    pub async fn exporter(&self, options: Options) -> Result<Exporter, Error> {
        component::instantiate(self, crate::Mode::Exporter, options, None)
            .await
            .map(Exporter::new)
    }

    pub(crate) fn engine(&self) -> &Engine {
        &self.engine
    }

    pub(crate) fn pre(&self) -> &InstancePre<HostState> {
        &self.pre
    }

    pub(crate) fn label_arc(&self) -> Arc<str> {
        self.label.clone()
    }
}
