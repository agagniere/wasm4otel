//! An embeddable [wasmtime] host for wasm4otel plugins.
//!
//! This crate speaks the same core-wasm ABI v2 as the two Go hosts in
//! `go/`: the `env` imports (`host_log`, `push_<signal>`,
//! `interruptible_sleep_ms`, `get_config`) and the `wasm4otel_`-prefixed
//! exports. A plugin cannot tell which host it runs under.
//!
//! Unlike the Go hosts, it knows nothing about the collector. It owns the
//! wasm side — compiling, instantiating, the lifecycle, the alloc → write →
//! call → free dance — and hands OTLP protobuf bytes in and out. Wiring
//! those bytes into a pipeline is the embedding application's job.
//!
//! # One engine, many plugins
//!
//! A [`Runtime`] holds one [`wasmtime::Engine`] and one import linker, and
//! is meant to be created once per process. Every plugin is compiled
//! against it once ([`Runtime::load`] returns a cheaply cloneable
//! [`Plugin`]), and every role a plugin is wired as gets its own
//! instance, with its own linear memory, from that one compiled module:
//!
//! ```no_run
//! # async fn example() -> Result<(), wasm4otel::Error> {
//! use wasm4otel::{Options, Runtime, RuntimeMode, Signal};
//!
//! let runtime = Runtime::new(RuntimeMode::Auto)?;
//! let plugin = runtime.load("severity_filter.wasm")?;
//!
//! let mut processor = plugin.processor(Options::default()).await?;
//! processor.supports(Signal::Logs)?;
//! processor.start().await?;
//! let output = processor.process(Signal::Logs, b"...").await?;
//! processor.shutdown().await;
//! # Ok(())
//! # }
//! ```
//!
//! # Roles
//!
//! Each role is its own type, so what a role may do is checked at compile
//! time rather than by branching on a mode:
//!
//! - [`Receiver`]: runs `wasm4otel_receive` until shutdown and forwards
//!   each `push_<signal>` to a [`Consumer`].
//! - [`Processor`]: hands a batch to `wasm4otel_process_<signal>` and
//!   returns whatever the guest pushed during that call.
//! - [`Exporter`]: hands a batch to the terminal
//!   `wasm4otel_export_<signal>`.
//!
//! # Async
//!
//! Every guest call is async. The guest runs on a wasmtime fiber, so a
//! host import can await without blocking a thread: `push_<signal>` in a
//! receiver awaits the [`Consumer`], which is where backpressure comes
//! from, and `interruptible_sleep_ms` is a timer raced against shutdown.
//! The timer needs a Tokio runtime.
//!
//! A guest that computes for a long time without calling the host still
//! occupies the thread polling it: there are no epoch or fuel limits yet.
//!
//! [wasmtime]: https://wasmtime.dev

#![deny(missing_docs)]

mod component;
mod error;
mod host;
mod runtime;

pub use self::component::{Exporter, Options, Processor, Receiver};
pub use self::error::Error;
pub use self::host::{BoxError, BoxFuture, Consumer};
pub use self::runtime::{Plugin, Runtime, RuntimeMode};

use std::fmt;

/// The telemetry signal a payload carries.
///
/// The payload itself is always an OTLP protobuf: `LogsData`,
/// `MetricsData` or `TracesData`. The OTLP export requests
/// (`ExportLogsServiceRequest` and friends) have the same wire encoding,
/// so their bytes can be passed as-is.
#[derive(Clone, Copy, Debug, Eq, Hash, PartialEq)]
pub enum Signal {
    /// Logs, as an OTLP `LogsData`.
    Logs,
    /// Metrics, as an OTLP `MetricsData`.
    Metrics,
    /// Traces, as an OTLP `TracesData`.
    Traces,
}

impl Signal {
    /// All three signals, in a fixed order.
    pub const ALL: [Signal; 3] = [Signal::Logs, Signal::Metrics, Signal::Traces];

    const fn index(self) -> usize {
        match self {
            Signal::Logs => 0,
            Signal::Metrics => 1,
            Signal::Traces => 2,
        }
    }

    const fn as_str(self) -> &'static str {
        match self {
            Signal::Logs => "logs",
            Signal::Metrics => "metrics",
            Signal::Traces => "traces",
        }
    }
}

impl fmt::Display for Signal {
    fn fmt(&self, f: &mut fmt::Formatter<'_>) -> fmt::Result {
        f.write_str(self.as_str())
    }
}

/// The role a plugin instance plays, which decides the exports it needs.
///
/// Reported in errors; the role types ([`Receiver`], [`Processor`],
/// [`Exporter`]) are what select it.
#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub enum Mode {
    /// Drives its own loop and pushes telemetry.
    Receiver,
    /// Transforms each batch handed to it.
    Processor,
    /// Terminally consumes each batch handed to it.
    Exporter,
}

impl fmt::Display for Mode {
    fn fmt(&self, f: &mut fmt::Formatter<'_>) -> fmt::Result {
        f.write_str(match self {
            Mode::Receiver => "receiver",
            Mode::Processor => "processor",
            Mode::Exporter => "exporter",
        })
    }
}
