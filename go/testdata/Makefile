# Test fixtures for the ABI v2 conformance suite, shared by both
# hosts: go/wazero and go/wazy run the same tests against the same
# .wasm files, so a fixture change lands on both at once.
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
