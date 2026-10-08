use std::path::PathBuf;

use crate::{Mode, Signal};

/// Everything that can go wrong while loading or driving a plugin.
///
/// Messages follow the Go hosts' wording, so an operator sees the same
/// sentence whichever host they run.
#[derive(Debug, thiserror::Error)]
#[non_exhaustive]
pub enum Error {
    /// The runtime could not be created with the requested settings.
    #[error("wasm4otel: {0}")]
    Runtime(String),

    /// The plugin path was empty.
    #[error("wasm4otel: plugin path must be set")]
    EmptyPath,

    /// The plugin file could not be read.
    #[error("wasm4otel: unable to read plugin {path}: {source}")]
    Read {
        /// The path that was configured.
        path: PathBuf,
        /// The underlying I/O error.
        source: std::io::Error,
    },

    /// The plugin is not a valid module, or imports something this host does
    /// not provide.
    #[error("wasm4otel: unable to load plugin {plugin}: {source:#}")]
    Compile {
        /// The plugin's label, usually its path.
        plugin: String,
        /// The error reported by wasmtime.
        source: wasmtime::Error,
    },

    /// Instantiating the plugin, or running its `_start` / `_initialize`
    /// entry point, failed.
    #[error("wasm4otel {mode}: unable to instantiate plugin: {source:#}")]
    Instantiate {
        /// The role the plugin was being instantiated for.
        mode: Mode,
        /// The error reported by wasmtime.
        source: wasmtime::Error,
    },

    /// An ABI export exists but has the wrong signature.
    #[error("wasm4otel {mode}: plugin export {export} has the wrong signature")]
    Signature {
        /// The role the plugin was instantiated for.
        mode: Mode,
        /// The ABI name of the export.
        export: &'static str,
    },

    /// The plugin lacks an export that its role needs regardless of signal.
    #[error("wasm4otel {mode}: plugin must export {what}")]
    ModeExport {
        /// The role the plugin was instantiated for.
        mode: Mode,
        /// The missing export(s), as a phrase.
        what: &'static str,
    },

    /// The plugin cannot serve this signal in the role it was instantiated for.
    #[error(
        "wasm4otel {mode}: plugin does not export {export}; it does not support the {signal} signal in this role"
    )]
    SignalExport {
        /// The role the plugin was instantiated for.
        mode: Mode,
        /// The ABI name of the missing export.
        export: &'static str,
        /// The signal that was asked for.
        signal: Signal,
    },

    /// `wasm4otel_setup` returned 2: the operator's `plugin_config` is wrong.
    #[error("wasm4otel {mode}: plugin rejected plugin_config as invalid (see plugin logs)")]
    InvalidConfig {
        /// The role the plugin was instantiated for.
        mode: Mode,
    },

    /// `wasm4otel_setup` returned another non-zero code.
    #[error("wasm4otel {mode}: plugin setup failed with code {code} (see plugin logs)")]
    SetupFailed {
        /// The role the plugin was instantiated for.
        mode: Mode,
        /// The code the guest returned.
        code: u32,
    },

    /// `wasm4otel_start` or `wasm4otel_receive` returned a non-zero code.
    #[error("wasm4otel {mode}: plugin {export} failed with code {code} (see plugin logs)")]
    StartFailed {
        /// The role the plugin was instantiated for.
        mode: Mode,
        /// The ABI name of the export.
        export: &'static str,
        /// The code the guest returned.
        code: u32,
    },

    /// A guest call trapped. The instance is poisoned from then on.
    #[error("wasm4otel {mode}: {export} trapped: {source:#}")]
    Trap {
        /// The role the plugin was instantiated for.
        mode: Mode,
        /// The ABI name of the export that trapped.
        export: &'static str,
        /// The trap reported by wasmtime.
        source: wasmtime::Error,
    },

    /// A previous call trapped, so the instance refuses further calls.
    #[error("wasm4otel: component is poisoned (prior trap)")]
    Poisoned,

    /// The batch does not fit in a 32-bit linear memory.
    #[error("wasm4otel: batch of {0} bytes is too large for a 32-bit guest")]
    PayloadTooLarge(usize),

    /// `wasm4otel_alloc` returned 0.
    #[error("wasm4otel: guest alloc returned 0 for {0} bytes")]
    GuestAlloc(u32),

    /// `wasm4otel_alloc` returned a region outside the guest's memory.
    #[error("wasm4otel: memory write out of range: ptr={ptr} size={size}")]
    OutOfRange {
        /// The pointer the guest returned.
        ptr: u32,
        /// The size that was requested.
        size: u32,
    },

    /// A batch export returned a non-zero code.
    #[error("wasm4otel: guest {export} returned {code}")]
    GuestReturned {
        /// The ABI name of the export.
        export: &'static str,
        /// The code the guest returned.
        code: u32,
    },
}
