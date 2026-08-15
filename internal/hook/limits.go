package hook

// Limits contains every configurable and runtime bound imposed by schema v1.
// A value returned by DefaultLimits is an independent copy.
type Limits struct {
	YAMLBytes             int
	RulesPerFile          int
	PredicatesPerRule     int
	CommandBytes          int
	CommandEnvCount       int
	EnvKeyBytes           int
	EnvValueBytes         int
	EventJSONBytes        int
	CommandStdoutBytes    int
	CommandStderrBytes    int
	HTTPURLBytes          int
	HTTPHeaderCount       int
	HTTPHeaderNameBytes   int
	HTTPHeaderValueBytes  int
	HTTPRequestBytes      int
	HTTPResponseBytes     int
	HTTPErrorPreviewBytes int
	HTTPRedirects         int
	PromptTemplateBytes   int
	PromptFragmentBytes   int
	PromptOwnerBytes      int
	SubAgentNameBytes     int
	SubAgentInputBytes    int
	DenyReasonBytes       int
	DiagnosticBytes       int
}

// DefaultLimits returns the immutable production defaults as a value copy.
func DefaultLimits() Limits {
	return Limits{
		YAMLBytes:             256 << 10,
		RulesPerFile:          256,
		PredicatesPerRule:     32,
		CommandBytes:          16 << 10,
		CommandEnvCount:       64,
		EnvKeyBytes:           128,
		EnvValueBytes:         8 << 10,
		EventJSONBytes:        1 << 20,
		CommandStdoutBytes:    32 << 10,
		CommandStderrBytes:    32 << 10,
		HTTPURLBytes:          2 << 10,
		HTTPHeaderCount:       32,
		HTTPHeaderNameBytes:   128,
		HTTPHeaderValueBytes:  8 << 10,
		HTTPRequestBytes:      1 << 20,
		HTTPResponseBytes:     64 << 10,
		HTTPErrorPreviewBytes: 1 << 10,
		HTTPRedirects:         3,
		PromptTemplateBytes:   64 << 10,
		PromptFragmentBytes:   128 << 10,
		PromptOwnerBytes:      256 << 10,
		SubAgentNameBytes:     64,
		SubAgentInputBytes:    64 << 10,
		DenyReasonBytes:       2 << 10,
		DiagnosticBytes:       2 << 10,
	}
}

func normalizeLimits(l Limits) Limits {
	d := DefaultLimits()
	if l.YAMLBytes > 0 {
		d.YAMLBytes = l.YAMLBytes
	}
	if l.RulesPerFile > 0 {
		d.RulesPerFile = l.RulesPerFile
	}
	if l.PredicatesPerRule > 0 {
		d.PredicatesPerRule = l.PredicatesPerRule
	}
	if l.CommandBytes > 0 {
		d.CommandBytes = l.CommandBytes
	}
	if l.CommandEnvCount > 0 {
		d.CommandEnvCount = l.CommandEnvCount
	}
	if l.EnvKeyBytes > 0 {
		d.EnvKeyBytes = l.EnvKeyBytes
	}
	if l.EnvValueBytes > 0 {
		d.EnvValueBytes = l.EnvValueBytes
	}
	if l.EventJSONBytes > 0 {
		d.EventJSONBytes = l.EventJSONBytes
	}
	if l.CommandStdoutBytes > 0 {
		d.CommandStdoutBytes = l.CommandStdoutBytes
	}
	if l.CommandStderrBytes > 0 {
		d.CommandStderrBytes = l.CommandStderrBytes
	}
	if l.HTTPURLBytes > 0 {
		d.HTTPURLBytes = l.HTTPURLBytes
	}
	if l.HTTPHeaderCount > 0 {
		d.HTTPHeaderCount = l.HTTPHeaderCount
	}
	if l.HTTPHeaderNameBytes > 0 {
		d.HTTPHeaderNameBytes = l.HTTPHeaderNameBytes
	}
	if l.HTTPHeaderValueBytes > 0 {
		d.HTTPHeaderValueBytes = l.HTTPHeaderValueBytes
	}
	if l.HTTPRequestBytes > 0 {
		d.HTTPRequestBytes = l.HTTPRequestBytes
	}
	if l.HTTPResponseBytes > 0 {
		d.HTTPResponseBytes = l.HTTPResponseBytes
	}
	if l.HTTPErrorPreviewBytes > 0 {
		d.HTTPErrorPreviewBytes = l.HTTPErrorPreviewBytes
	}
	if l.HTTPRedirects > 0 {
		d.HTTPRedirects = l.HTTPRedirects
	}
	if l.PromptTemplateBytes > 0 {
		d.PromptTemplateBytes = l.PromptTemplateBytes
	}
	if l.PromptFragmentBytes > 0 {
		d.PromptFragmentBytes = l.PromptFragmentBytes
	}
	if l.PromptOwnerBytes > 0 {
		d.PromptOwnerBytes = l.PromptOwnerBytes
	}
	if l.SubAgentNameBytes > 0 {
		d.SubAgentNameBytes = l.SubAgentNameBytes
	}
	if l.SubAgentInputBytes > 0 {
		d.SubAgentInputBytes = l.SubAgentInputBytes
	}
	if l.DenyReasonBytes > 0 {
		d.DenyReasonBytes = l.DenyReasonBytes
	}
	if l.DiagnosticBytes > 0 {
		d.DiagnosticBytes = l.DiagnosticBytes
	}
	return d
}
