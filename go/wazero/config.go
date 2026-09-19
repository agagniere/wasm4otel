package wasm4otel

// Credit: https://github.com/otelwasm/otelwasm/blob/main/wasmplugin/config.go
// License: Apache License 2.0

import (
	"fmt"

	"go.opentelemetry.io/collector/component"
)

// PluginConfig is a generic configuration type that can be passed to WASM modules
type PluginConfig map[string]interface{}

// Config defines the common configuration for WASM components
type Config struct {
	// Path to the WASM module file
	Path string `mapstructure:"path"`

	// PluginConfig is the configuration to be passed to the WASM module
	PluginConfig PluginConfig `mapstructure:"plugin_config"`
}

// Validate validates the configuration
func (cfg *Config) Validate() error {
	if cfg.Path == "" {
		return fmt.Errorf("path is required")
	}
	return nil
}

func DefaultConfig() component.Config {
	return Config{
		Path: "",
	}
}
