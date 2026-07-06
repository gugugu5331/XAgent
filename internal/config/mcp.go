package config

import "fmt"

const (
	MCPTransportStdio = "stdio"
	MCPTransportHTTP  = "http"
)

type MCPConfig struct {
	DefaultTimeoutMS int64                      `yaml:"default_timeout_ms"`
	Servers          map[string]MCPServerConfig `yaml:"servers"`
	Diagnostics      []MCPDiagnostic            `yaml:"-"`
}

type MCPServerConfig struct {
	Disabled  bool              `yaml:"disabled"`
	Type      string            `yaml:"type"`
	Command   string            `yaml:"command,omitempty"`
	Args      []string          `yaml:"args,omitempty"`
	Env       map[string]string `yaml:"env,omitempty"`
	URL       string            `yaml:"url,omitempty"`
	Headers   map[string]string `yaml:"headers,omitempty"`
	TimeoutMS int64             `yaml:"timeout_ms,omitempty"`
	Source    string            `yaml:"-"`
}

type MCPDiagnostic struct {
	Server  string
	Source  string
	Message string
}

func MergeMCPConfig(user MCPConfig, project MCPConfig) MCPConfig {
	merged := MCPConfig{DefaultTimeoutMS: user.DefaultTimeoutMS, Servers: map[string]MCPServerConfig{}}
	for name, server := range user.Servers {
		server.Source = sourceOrDefault(server.Source, "user")
		merged.Servers[name] = server
	}
	if project.DefaultTimeoutMS > 0 {
		merged.DefaultTimeoutMS = project.DefaultTimeoutMS
	}
	for name, server := range project.Servers {
		server.Source = sourceOrDefault(server.Source, "project")
		merged.Servers[name] = server
	}
	return merged
}

func (c *MCPConfig) addDiagnostic(server string, source string, format string, args ...any) {
	c.Diagnostics = append(c.Diagnostics, MCPDiagnostic{Server: server, Source: source, Message: fmt.Sprintf(format, args...)})
}

func sourceOrDefault(source string, fallback string) string {
	if source != "" {
		return source
	}
	return fallback
}
