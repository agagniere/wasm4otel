;; An ABI v2 plugin that only serves the logs processor role. Used to
;; check that validateModeExports and ValidateXExport reject it when an
;; operator wires it under `receivers:` or asks for a signal it never
;; declared.
;;
;; Regenerate processor_only_plugin.wasm with `make -C testdata`.
(module
  (import "env" "push_logs" (func $push_logs (param i32 i32) (result i32)))

  (memory (export "memory") 4)

  (global $bump (mut i32) (i32.const 65536))

  (func (export "wasm4otel_alloc") (param $size i32) (result i32)
    (local $ptr i32)
    (local.set $ptr (global.get $bump))
    (if (i32.gt_u
          (i32.add (local.get $ptr) (local.get $size))
          (i32.mul (memory.size) (i32.const 65536)))
      (then (return (i32.const 0))))
    (global.set $bump (i32.add (local.get $ptr) (local.get $size)))
    (local.get $ptr))

  (func (export "wasm4otel_free") (param i32 i32))

  (func (export "wasm4otel_process_logs") (param $ptr i32) (param $size i32) (result i32)
    (drop (call $push_logs (local.get $ptr) (local.get $size)))
    (i32.const 0))
)
