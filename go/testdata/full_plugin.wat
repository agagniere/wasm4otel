;; A minimal ABI v2 plugin exporting every role: a receive loop, a
;; logs processor, and a logs exporter. Used by the Go tests to drive
;; the wazy host through each lifecycle without pulling in a Zig build.
;;
;; The processor path forwards the batch it was handed straight back to
;; push_logs, which round-trips the OTLP payload through host memory and
;; proves alloc / write / call / free all line up.
;;
;; Regenerate full_plugin.wasm with `make -C testdata`.
(module
  (import "env" "host_log" (func $host_log (param i32 i32 i32)))
  (import "env" "push_logs" (func $push_logs (param i32 i32) (result i32)))
  (import "env" "push_metrics" (func $push_metrics (param i32 i32) (result i32)))
  (import "env" "push_traces" (func $push_traces (param i32 i32) (result i32)))
  (import "env" "interruptible_sleep_ms" (func $sleep (param i32) (result i32)))
  (import "env" "get_config" (func $get_config (param i32 i32) (result i32)))

  (memory (export "memory") 4)

  ;; Bump allocator over everything past the first page. The first page
  ;; holds the static data below.
  (global $bump (mut i32) (i32.const 65536))

  (data (i32.const 0) "v2 fixture ready")

  ;; A hand-encoded, minimal OTLP LogsData holding one empty LogRecord:
  ;;   resource_logs (field 1, len 4)
  ;;     scope_logs  (field 2, len 2)
  ;;       log_records (field 2, len 0)
  ;; The receive loop pushes this so the host-side test can observe that
  ;; the loop actually ran, rather than only that Shutdown returned.
  (data (i32.const 16) "\0a\04\12\02\12\00")

  (func (export "wasm4otel_alloc") (param $size i32) (result i32)
    (local $ptr i32)
    (local.set $ptr (global.get $bump))
    ;; Refuse rather than hand back a pointer past the end of memory —
    ;; returning 0 is the ABI's "allocation failed" signal.
    (if (i32.gt_u
          (i32.add (local.get $ptr) (local.get $size))
          (i32.mul (memory.size) (i32.const 65536)))
      (then (return (i32.const 0))))
    (global.set $bump (i32.add (local.get $ptr) (local.get $size)))
    (local.get $ptr))

  ;; This fixture never reuses memory, so free is a no-op.
  (func (export "wasm4otel_free") (param i32 i32))

  ;; Log at zap's info level (0) to prove host_log is wired.
  (func (export "wasm4otel_setup") (result i32)
    (call $host_log (i32.const 0) (i32.const 0) (i32.const 16))
    (i32.const 0))

  (func (export "wasm4otel_start") (result i32) (i32.const 0))

  (func (export "wasm4otel_shutdown"))

  ;; Push one batch to prove the loop is running, then sleep until the
  ;; host cancels the component context, which makes
  ;; interruptible_sleep_ms return non-zero.
  (func (export "wasm4otel_receive") (result i32)
    (drop (call $push_logs (i32.const 16) (i32.const 6)))
    (block $done
      (loop $again
        (br_if $done (call $sleep (i32.const 5)))
        (br $again)))
    (i32.const 0))

  (func (export "wasm4otel_process_logs") (param $ptr i32) (param $size i32) (result i32)
    (drop (call $push_logs (local.get $ptr) (local.get $size)))
    (i32.const 0))

  (func (export "wasm4otel_process_metrics") (param $ptr i32) (param $size i32) (result i32)
    (drop (call $push_metrics (local.get $ptr) (local.get $size)))
    (i32.const 0))

  (func (export "wasm4otel_process_traces") (param $ptr i32) (param $size i32) (result i32)
    (drop (call $push_traces (local.get $ptr) (local.get $size)))
    (i32.const 0))

  ;; Exporters are terminal: consume the batch and report success
  ;; without forwarding it anywhere.
  (func (export "wasm4otel_export_logs") (param i32 i32) (result i32) (i32.const 0))
)
