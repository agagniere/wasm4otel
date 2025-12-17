SourceRoot := foo.zig
Sources    := log.zig
Module     := $(SourceRoot:%.zig=%.wasm)
Component  := $(Module:%.wasm=%.comp.wasm)
TargetArch := wasm32-wasi

ZIG          ?= zig
WASM_RUNTIME ?= wasmtime
WASM_TOOLS   ?= wasm-tools

WASI_P1P2_ADAPTER ?= wasi_snapshot_preview1.reactor.wasm

# ----- Phony targets -----

build: $(Module)

component: $(Component)

test: $(SourceRoot)
	$(ZIG) test -target $(TargetArch) $< --test-cmd $(WASM_RUNTIME) --test-cmd-bin

clean:
	$(RM) $(Component) $(Module)

.PHONY: build component test clean

# ----- Concrete targets -----

$(Module): %.wasm: %.zig $(Sources)
	$(ZIG) build-exe -O ReleaseSmall \
		-target $(TargetArch) -mexec-model=reactor \
		-rdynamic \
		$<

$(Component): %.comp.wasm: %.wasm
	$(WASM_TOOLS) component new $< --adapt $(WASI_P1P2_ADAPTER) -o $@
