# Test fixtures for the wazy host's ABI v2 conformance tests.
#
# The .wat files are the source of truth; the .wasm files next to them
# are committed so `go test ./...` stays hermetic and needs no
# toolchain beyond Go. Regenerate after editing a .wat:
#
#     make -C testdata
#
# Requires wasm-tools (https://github.com/bytecodealliance/wasm-tools).

WATS := $(wildcard *.wat)
WASMS := $(WATS:.wat=.wasm)

all: $(WASMS)

%.wasm: %.wat
	wasm-tools parse -o $@ $<

clean:
	rm -f $(WASMS)

.PHONY: all clean
