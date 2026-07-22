package tools

// Config is a minimal config for the tool registry.
type Config struct {
	WorkspaceDir string
}

// NewRegistryWithDefaults creates a new registry with all default tools registered.
func NewRegistryWithDefaults(cfg *Config) *Registry {
	registry := NewRegistry()

	defaultTools := []Tool{
		&GetTodayTool{},
	}

	for _, tool := range defaultTools {
		_ = registry.Register(tool)
	}
	return registry
}
