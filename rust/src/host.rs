//! The `env` host imports, and the per-instance state they read.

use std::future::Future;
use std::pin::Pin;
use std::sync::Arc;
use std::time::Duration;

use tokio::sync::watch;
use tracing::{debug, error, info, warn};
use wasmtime::{Caller, Linker};
use wasmtime_wasi::p1::WasiP1Ctx;

use crate::{Mode, Signal};

/// A boxed, sendable future, as returned by [`Consumer::consume`].
pub type BoxFuture<'a, T> = Pin<Box<dyn Future<Output = T> + Send + 'a>>;

/// A boxed, sendable error, as returned by [`Consumer::consume`].
pub type BoxError = Box<dyn std::error::Error + Send + Sync>;

/// Where a [`Receiver`](crate::Receiver) sends what its guest pushes.
///
/// This is the Go hosts' `NextConsumer`. The guest waits for `consume` to
/// finish before `push_<signal>` returns, so a consumer that awaits
/// downstream capacity slows the guest down instead of buffering without
/// bound.
pub trait Consumer: Send + Sync + 'static {
    /// Accepts one OTLP payload for `signal`.
    ///
    /// The payload has passed a structural protobuf check but has not been
    /// decoded. Returning an error makes `push_<signal>` return 4 to the
    /// guest ("downstream rejected the batch").
    fn consume(&self, signal: Signal, payload: Vec<u8>) -> BoxFuture<'_, Result<(), BoxError>>;
}

/// `push_<signal>` return codes, shared with the Go hosts.
mod push_rc {
    pub const OK: u32 = 0;
    pub const BAD_MEMORY: u32 = 1;
    pub const MALFORMED: u32 = 2;
    pub const DEAD_END: u32 = 3;
    pub const REJECTED: u32 = 4;
}

/// What a processor call collects: the guest's pushes for one signal.
pub(crate) struct Capture {
    pub signal: Signal,
    /// Every push appended back to back. Concatenating serialized
    /// `LogsData` messages yields one `LogsData` holding all their
    /// `resource_logs`, which is what the Go hosts get from
    /// `MoveAndAppendTo` without decoding anything.
    pub bytes: Vec<u8>,
}

/// The store data of one plugin instance.
pub(crate) struct HostState {
    pub wasi: WasiP1Ctx,
    pub mode: Mode,
    /// Used to tag guest logs, usually the plugin path.
    pub label: Arc<str>,
    /// The operator's `plugin_config`, already encoded as JSON.
    pub plugin_config: Option<Arc<[u8]>>,
    /// Set for receivers only.
    pub consumer: Option<Arc<dyn Consumer>>,
    /// Set for exactly the span of one processor call.
    pub capture: Option<Capture>,
    /// Flips to `true` on shutdown, which wakes `interruptible_sleep_ms`.
    pub shutdown: watch::Receiver<bool>,
}

/// Defines the `env` module on `linker`.
///
/// The linker is shared by every instance in the runtime, so these
/// closures hold no state of their own: everything comes from the
/// instance's [`HostState`].
pub(crate) fn add_to_linker(linker: &mut Linker<HostState>) -> wasmtime::Result<()> {
    linker.func_wrap("env", "host_log", host_log)?;
    linker.func_wrap("env", "get_config", get_config)?;
    for (name, signal) in [
        ("push_logs", Signal::Logs),
        ("push_metrics", Signal::Metrics),
        ("push_traces", Signal::Traces),
    ] {
        linker.func_wrap_async("env", name, move |caller, (ptr, size): (u32, u32)| {
            Box::new(push(caller, signal, ptr, size))
        })?;
    }
    linker.func_wrap_async("env", "interruptible_sleep_ms", |caller, (ms,): (u32,)| {
        Box::new(interruptible_sleep(caller, ms))
    })?;
    Ok(())
}

/// `host_log(level, ptr, size)`: the level is a zap level (debug = -1,
/// info = 0, warn = 1, error = 2, and the panic/fatal levels above). The
/// panic and fatal levels are logged as errors: a plugin does not get to
/// take the process down.
fn host_log(mut caller: Caller<'_, HostState>, level: i32, ptr: u32, size: u32) {
    let label = caller.data().label.clone();
    let Some(bytes) = read_guest(&mut caller, ptr, size) else {
        error!(plugin = %label, ptr, size, "host_log: unable to read guest memory");
        return;
    };
    let message = String::from_utf8_lossy(&bytes);
    match level {
        ..=-1 => debug!(plugin = %label, "{message}"),
        0 => info!(plugin = %label, "{message}"),
        1 => warn!(plugin = %label, "{message}"),
        _ => error!(plugin = %label, "{message}"),
    }
}

/// `get_config(ptr, size)`: see [`write_config`].
fn get_config(mut caller: Caller<'_, HostState>, ptr: u32, size: u32) -> u32 {
    let label = caller.data().label.clone();
    let config = caller.data().plugin_config.clone();
    let Some(memory) = caller.get_export("memory").and_then(|e| e.into_memory()) else {
        error!(plugin = %label, "get_config: plugin exports no memory");
        return 0;
    };
    match write_config(config.as_deref(), memory.data_mut(&mut caller), ptr, size) {
        Some(total) => total,
        None => {
            error!(plugin = %label, ptr, size, "get_config: memory write failed");
            0
        }
    }
}

/// Writes the JSON config into guest memory, all or nothing, and returns its
/// true size. `None` means the region is out of bounds.
///
/// - no config: returns 0 and writes nothing;
/// - `size` too small: returns the true size and writes nothing, so the
///   guest can allocate and call again — JSON cannot be parsed partially;
/// - otherwise: writes the document at `ptr` and returns its size.
///
/// Probing with `(0, 0)` therefore reports the size without touching memory.
pub(crate) fn write_config(
    config: Option<&[u8]>,
    memory: &mut [u8],
    ptr: u32,
    size: u32,
) -> Option<u32> {
    let Some(config) = config.filter(|c| !c.is_empty()) else {
        return Some(0);
    };
    let total = u32::try_from(config.len()).ok()?;
    if size < total {
        return Some(total);
    }
    let start = ptr as usize;
    memory
        .get_mut(start..start.checked_add(config.len())?)?
        .copy_from_slice(config);
    Some(total)
}

/// `push_<signal>(ptr, size)`: a receiver forwards to its consumer, a
/// processor appends to the current call's capture, an exporter has
/// nowhere to push to.
async fn push(mut caller: Caller<'_, HostState>, signal: Signal, ptr: u32, size: u32) -> u32 {
    let label = caller.data().label.clone();
    let Some(payload) = read_guest(&mut caller, ptr, size) else {
        error!(plugin = %label, ptr, size, "push_{signal}: unable to read guest memory");
        return push_rc::BAD_MEMORY;
    };
    if !is_wellformed_protobuf(&payload) {
        error!(plugin = %label, size, "push_{signal}: payload is not a protobuf message");
        return push_rc::MALFORMED;
    }

    let state = caller.data_mut();
    match state.mode {
        Mode::Processor => match &mut state.capture {
            Some(capture) if capture.signal == signal => {
                capture.bytes.extend_from_slice(&payload);
                push_rc::OK
            }
            _ => {
                error!(plugin = %label, "plugin is pushing {signal} to a dead-end");
                push_rc::DEAD_END
            }
        },
        Mode::Receiver => {
            let Some(consumer) = state.consumer.clone() else {
                error!(plugin = %label, "plugin is pushing {signal} to a dead-end");
                return push_rc::DEAD_END;
            };
            match consumer.consume(signal, payload).await {
                Ok(()) => push_rc::OK,
                Err(e) => {
                    error!(plugin = %label, size, error = %e, "downstream consumer rejected batch");
                    push_rc::REJECTED
                }
            }
        }
        Mode::Exporter => {
            error!(plugin = %label, "plugin is pushing {signal} to a dead-end");
            push_rc::DEAD_END
        }
    }
}

/// `interruptible_sleep_ms(ms)`: 0 when the duration elapsed, 1 when
/// shutdown started first (or had already started).
async fn interruptible_sleep(caller: Caller<'_, HostState>, ms: u32) -> u32 {
    let mut shutdown = caller.data().shutdown.clone();
    tokio::select! {
        // `wait_for` checks the current value first, and errors once the
        // sender is gone: both mean the component is shutting down.
        _ = shutdown.wait_for(|stopping| *stopping) => 1,
        () = tokio::time::sleep(Duration::from_millis(ms.into())) => 0,
    }
}

/// Copies `size` bytes at `ptr` out of the guest's exported memory.
fn read_guest(caller: &mut Caller<'_, HostState>, ptr: u32, size: u32) -> Option<Vec<u8>> {
    let memory = caller.get_export("memory")?.into_memory()?;
    let start = ptr as usize;
    let end = start.checked_add(size as usize)?;
    memory.data(&caller).get(start..end).map(<[u8]>::to_vec)
}

/// Checks that `bytes` is a sequence of well-formed top-level protobuf
/// fields, without knowing the schema.
///
/// This is the host's stand-in for the Go hosts' full `ProtoUnmarshaler`:
/// it rejects garbage and truncated payloads (`push_<signal>` returns 2),
/// but does not look inside length-delimited fields. Decoding is left to
/// whoever consumes the bytes, which is the point of passing bytes.
pub(crate) fn is_wellformed_protobuf(mut bytes: &[u8]) -> bool {
    fn varint(bytes: &mut &[u8]) -> Option<u64> {
        let mut value = 0u64;
        for shift in (0..64).step_by(7) {
            let (&byte, rest) = bytes.split_first()?;
            *bytes = rest;
            value |= u64::from(byte & 0x7f) << shift;
            if byte & 0x80 == 0 {
                return Some(value);
            }
        }
        None
    }
    fn skip(bytes: &mut &[u8], len: u64) -> Option<()> {
        let len = usize::try_from(len).ok()?;
        *bytes = bytes.get(len..)?;
        Some(())
    }

    while !bytes.is_empty() {
        let Some(tag) = varint(&mut bytes) else {
            return false;
        };
        if tag >> 3 == 0 {
            return false;
        }
        let ok = match tag & 7 {
            0 => varint(&mut bytes).map(drop),
            1 => skip(&mut bytes, 8),
            2 => varint(&mut bytes).and_then(|len| skip(&mut bytes, len)),
            5 => skip(&mut bytes, 4),
            // Groups (3, 4) are deprecated and absent from OTLP.
            _ => None,
        };
        if ok.is_none() {
            return false;
        }
    }
    true
}

#[cfg(test)]
mod tests {
    use super::*;

    /// The two-step get_config contract: a (0, 0) probe reports the size
    /// without touching memory, an undersized buffer still reports the true
    /// size and writes nothing, and a large enough buffer gets the bytes.
    #[test]
    fn get_config_probe_then_read() {
        let config = br#"{"level":"warn"}"#;
        let size = config.len() as u32;
        let mut memory = vec![0u8; 64];

        assert_eq!(write_config(Some(config), &mut memory, 0, 0), Some(size));

        assert_eq!(
            write_config(Some(config), &mut memory, 8, size - 1),
            Some(size)
        );
        assert!(
            memory.iter().all(|&b| b == 0),
            "undersized call wrote a truncated document"
        );

        assert_eq!(write_config(Some(config), &mut memory, 8, size), Some(size));
        assert_eq!(&memory[8..8 + config.len()], config);
    }

    #[test]
    fn get_config_without_config_returns_zero() {
        let mut memory = vec![0u8; 8];
        assert_eq!(write_config(None, &mut memory, 0, 8), Some(0));
    }

    #[test]
    fn get_config_out_of_bounds_writes_nothing() {
        let mut memory = vec![0u8; 8];
        assert_eq!(write_config(Some(b"{}"), &mut memory, 7, 2), None);
        assert_eq!(write_config(Some(b"{}"), &mut memory, u32::MAX, 2), None);
        assert!(memory.iter().all(|&b| b == 0));
    }

    #[test]
    fn wire_check_accepts_otlp_shapes() {
        assert!(is_wellformed_protobuf(b""));
        // LogsData { resource_logs { scope_logs { log_records {} } } }
        assert!(is_wellformed_protobuf(b"\x0a\x04\x12\x02\x12\x00"));
        // Two pushes back to back are still one well-formed message.
        assert!(is_wellformed_protobuf(
            b"\x0a\x04\x12\x02\x12\x00\x0a\x04\x12\x02\x12\x00"
        ));
        // Varint, fixed64 and fixed32 fields (unknown fields are legal).
        assert!(is_wellformed_protobuf(
            b"\x10\x96\x01\x19\x01\x02\x03\x04\x05\x06\x07\x08\x25\x01\x02\x03\x04"
        ));
    }

    #[test]
    fn wire_check_rejects_garbage() {
        // Length runs past the end.
        assert!(!is_wellformed_protobuf(b"\x0a\x05\x12\x02"));
        // Field number 0.
        assert!(!is_wellformed_protobuf(b"\x02\x00"));
        // Group wire type.
        assert!(!is_wellformed_protobuf(b"\x0b\x0c"));
        // Truncated varint.
        assert!(!is_wellformed_protobuf(b"\x08\x80"));
        assert!(!is_wellformed_protobuf(b"hello world"));
    }
}
