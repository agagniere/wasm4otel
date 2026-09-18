package wasm4otel

// Credit: https://github.com/otelwasm/otelwasm/blob/main/wasmplugin/config.go
// License: Apache License 2.0

import (
	"fmt"

	"go.opentelemetry.io/collector/component"
)

// PluginConfig is a generic configuration type that can be passed to WASM modules
type PluginConfig map[string]interface{}

// RuntimeMode selects which wazero execution backend runs the plugin.
// Both backends implement the same wasm feature set, so this only
// trades throughput against portability — it never changes what a
// plugin is able to do.
//
// The name and the two shared values match otelwasm's `runtime.mode`,
// so a config written for one loads under the other.
type RuntimeMode string

const (
	// RuntimeModeInterpreter executes bytecode in pure Go. Slowest — at
	// least an order of magnitude behind the compiler on our plugins —
	// but runs anywhere Go runs and needs no executable memory. Ask
	// for it explicitly to rule native codegen out.
	RuntimeModeInterpreter RuntimeMode = "interpreter"

	// RuntimeModeCompiled compiles each module to native code at load
	// time. Only amd64 and arm64 have a backend, and the host has to
	// allow mapping memory executable; where that does not hold,
	// wazero panics rather than degrading, taking the collector with
	// it. Pick this when the deployment target is known,
	// RuntimeModeAuto otherwise.
	RuntimeModeCompiled RuntimeMode = "compiled"

	// RuntimeModeAuto lets wazero probe the platform and take the
	// compiler where it works, the interpreter everywhere else. The
	// portable way to ask for native speed, and the default. No
	// otelwasm equivalent — a config naming it is ours only.
	RuntimeModeAuto RuntimeMode = "auto"
)

// orDefault maps the unset zero value onto the documented default, so
// a Config assembled in code rather than through DefaultConfig still
// names a mode.
func (mode RuntimeMode) orDefault() RuntimeMode {
	if mode == "" {
		return RuntimeModeAuto
	}
	return mode
}

// RuntimeConfig groups the settings that shape the wazero runtime
// rather than the plugin inside it. A sub-map of one field looks like
// overkill, but it is the shape otelwasm uses and the place anything
// else runtime-wide (memory limits, a shared runtime) would land.
type RuntimeConfig struct {
	// Mode selects the wazero execution backend
	Mode RuntimeMode `mapstructure:"mode,omitempty"`
}

// Config defines the common configuration for WASM components
type Config struct {
	// Path to the WASM module file
	Path string `mapstructure:"path"`

	// RuntimeConfig configures the wazero runtime itself
	RuntimeConfig RuntimeConfig `mapstructure:"runtime"`

	// PluginConfig is the configuration to be passed to the WASM module
	PluginConfig PluginConfig `mapstructure:"plugin_config"`
}

// Validate validates the configuration
func (cfg *Config) Validate() error {
	if cfg.Path == "" {
		return fmt.Errorf("path is required")
	}
	switch cfg.RuntimeConfig.Mode {
	case "", RuntimeModeInterpreter, RuntimeModeCompiled, RuntimeModeAuto:
	default:
		return fmt.Errorf("runtime mode: got %q, want %q, %q or %q",
			cfg.RuntimeConfig.Mode, RuntimeModeAuto, RuntimeModeInterpreter, RuntimeModeCompiled)
	}
	return nil
}

func DefaultConfig() component.Config {
	return Config{
		Path:          "",
		RuntimeConfig: RuntimeConfig{Mode: RuntimeModeAuto},
	}
}
