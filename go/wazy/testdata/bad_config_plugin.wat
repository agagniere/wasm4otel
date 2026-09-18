;; An ABI v2 plugin whose wasm4otel_setup rejects the operator's
;; plugin_config. Used to check that a guest-side validation failure
;; surfaces as an error out of the factory's createX — i.e. the
;; collector refuses to finish booting — rather than showing up at the
;; first batch.
;;
;; Regenerate bad_config_plugin.wasm with `make -C testdata`.
(module
  (memory (export "memory") 1)

  (func (export "wasm4otel_alloc") (param i32) (result i32) (i32.const 0))
  (func (export "wasm4otel_free") (param i32 i32))

  ;; 2 = invalid_config
  (func (export "wasm4otel_setup") (result i32) (i32.const 2))

  (func (export "wasm4otel_process_logs") (param i32 i32) (result i32) (i32.const 0))
)
