package wasm4otel

// Credit: https://github.com/otelwasm/otelwasm/blob/main/wasmplugin/config.go
// License: Apache License 2.0

import (
	"fmt"

	"go.opentelemetry.io/collector/component"
)

// PluginConfig is a generic configuration type that can be passed to WASM modules
type PluginConfig map[string]interface{}

// Engine selects which wazero execution backend runs the plugin. Both
// backends implement the same wasm feature set, so this only trades
// throughput against portability — it never changes what a plugin is
// able to do.
type Engine string

const (
	// EngineInterpreter executes bytecode in pure Go. Slowest — at
	// least an order of magnitude behind the compiler on our plugins —
	// but runs anywhere Go runs and needs no executable memory. Ask
	// for it explicitly to rule the compiler out.
	EngineInterpreter Engine = "interpreter"

	// EngineCompiler compiles each module to native code at load time.
	// Only amd64 and arm64 have a backend, and the host has to allow
	// mapping memory executable; where that does not hold, wazero
	// panics rather than degrading, taking the collector with it. Pick
	// this when the deployment target is known, EngineAuto otherwise.
	EngineCompiler Engine = "compiler"

	// EngineAuto lets wazero probe the platform and take the compiler
	// where it works, the interpreter everywhere else. The portable
	// way to ask for native speed, and the default.
	EngineAuto Engine = "auto"
)

// orDefault maps the unset zero value onto the documented default, so
// a Config assembled in code rather than through DefaultConfig still
// names an engine.
func (engine Engine) orDefault() Engine {
	if engine == "" {
		return EngineAuto
	}
	return engine
}

// Config defines the common configuration for WASM components
type Config struct {
	// Path to the WASM module file
	Path string `mapstructure:"path"`

	// Engine selects the wazero execution backend
	Engine Engine `mapstructure:"engine"`

	// PluginConfig is the configuration to be passed to the WASM module
	PluginConfig PluginConfig `mapstructure:"plugin_config"`
}

// Validate validates the configuration
func (cfg *Config) Validate() error {
	if cfg.Path == "" {
		return fmt.Errorf("path is required")
	}
	switch cfg.Engine {
	case "", EngineInterpreter, EngineCompiler, EngineAuto:
	default:
		return fmt.Errorf("engine: got %q, want %q, %q or %q",
			cfg.Engine, EngineInterpreter, EngineCompiler, EngineAuto)
	}
	return nil
}

func DefaultConfig() component.Config {
	return Config{
		Path:   "",
		Engine: EngineAuto,
	}
}
