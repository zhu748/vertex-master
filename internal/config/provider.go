package config

type ConfigProvider interface {
	PortAPI() int
	MaxRetries() int
	AdminPassword() string
	ProxyURL() string
	DebugPprof() bool
	DebugMode() bool

	DropMaxTokens() bool

	AggregateStream() bool
	MaxN() int
	MaxRequestMB() int
	MaxConcurrentRequests() int
	MaxSpillMB() int
	RequestTimeout() int

	VertexAPIKey() string
	CountTokensQuerySignature() string

	// SafetySettings returns the immutable map from the active configuration
	// snapshot. Callers must not mutate it.
	SafetySettings() map[string]string

	ParallelPoolEnabled() bool
	StickyNodePriority() bool
	ParallelPoolRetryEnabled() bool
	ParallelPoolSize() int
	ParallelPoolDelayDynamic() bool
	ParallelPoolDelayMs() int
	ProxyFailoverMaxAttempts() int
	ActiveNodeURI() string
	ParallelNodeTopK() int
	ProxyHealthCheckEnabled() bool
	ProxyHealthCheckIntervalMinutes() int
	ProxyHealthCheckBatchSize() int
	ProxyHealthCheckConcurrency() int
	ProxyHealthCheckTimeoutSeconds() int
	AllowPrivateSubscriptionURLs() bool
	AllowDomainSubscriptionProxies() bool
	ProxySubscriptionAllowProxyFallback() bool

	BackgroundImage() string
	FontSize() string
	FontColorType() string
	FontColor() string
	CustomBgPresets() []string
	AutoRefreshLogs() bool

	BaseModels() []string
	AliasMap() map[string]string
	ModelsWithFakeVariants() []string
	FakePrefixes() []string
	ResolveModelName(string) string

	ConfigDir() string
	ConfigPath() string
}

type ConfigWriter interface {
	WriteSettings(map[string]any) error
	WriteModels([]string, map[string]string) error
}

type dynamicConfig struct{}

func GetProvider() ConfigProvider { return dynamicConfig{} }

func (d dynamicConfig) PortAPI() int                      { return Load().PortAPI }
func (d dynamicConfig) MaxRetries() int                   { return Load().MaxRetries }
func (d dynamicConfig) AdminPassword() string             { return Load().AdminPassword }
func (d dynamicConfig) ProxyURL() string                  { return Load().ProxyURL }
func (d dynamicConfig) DebugPprof() bool                  { return Load().DebugPprof }
func (d dynamicConfig) DebugMode() bool                   { return Load().DebugMode }
func (d dynamicConfig) DropMaxTokens() bool               { return Load().DropMaxTokens }
func (d dynamicConfig) AggregateStream() bool             { return Load().AggregateStream }
func (d dynamicConfig) MaxN() int                         { return Load().MaxN }
func (d dynamicConfig) MaxRequestMB() int                 { return Load().MaxRequestMB }
func (d dynamicConfig) MaxConcurrentRequests() int        { return Load().MaxConcurrentRequests }
func (d dynamicConfig) MaxSpillMB() int                   { return Load().MaxSpillMB }
func (d dynamicConfig) RequestTimeout() int               { return Load().RequestTimeout }
func (d dynamicConfig) VertexAPIKey() string              { return Load().VertexAPIKey }
func (d dynamicConfig) CountTokensQuerySignature() string { return Load().CountTokensQuerySignature }
func (d dynamicConfig) SafetySettings() map[string]string { return Load().SafetySettings }
func (d dynamicConfig) ParallelPoolEnabled() bool         { return Load().ParallelPoolEnabled }
func (d dynamicConfig) StickyNodePriority() bool          { return Load().StickyNodePriority }
func (d dynamicConfig) ParallelPoolRetryEnabled() bool    { return Load().ParallelPoolRetryEnabled }
func (d dynamicConfig) ParallelPoolSize() int             { return Load().ParallelPoolSize }
func (d dynamicConfig) ParallelPoolDelayDynamic() bool    { return Load().ParallelPoolDelayDynamic }
func (d dynamicConfig) ParallelPoolDelayMs() int          { return Load().ParallelPoolDelayMs }
func (d dynamicConfig) ProxyFailoverMaxAttempts() int     { return Load().ProxyFailoverMaxAttempts }
func (d dynamicConfig) ActiveNodeURI() string             { return Load().ActiveNodeURI }
func (d dynamicConfig) ParallelNodeTopK() int             { return Load().ParallelNodeTopK }
func (d dynamicConfig) ProxyHealthCheckEnabled() bool     { return Load().ProxyHealthCheckEnabled }
func (d dynamicConfig) ProxyHealthCheckIntervalMinutes() int {
	return Load().ProxyHealthCheckIntervalMinutes
}
func (d dynamicConfig) ProxyHealthCheckBatchSize() int {
	return Load().ProxyHealthCheckBatchSize
}
func (d dynamicConfig) ProxyHealthCheckConcurrency() int {
	return Load().ProxyHealthCheckConcurrency
}
func (d dynamicConfig) ProxyHealthCheckTimeoutSeconds() int {
	return Load().ProxyHealthCheckTimeoutSeconds
}
func (d dynamicConfig) AllowPrivateSubscriptionURLs() bool {
	return Load().AllowPrivateSubscriptionURLs
}
func (d dynamicConfig) AllowDomainSubscriptionProxies() bool {
	return Load().AllowDomainSubscriptionProxies
}
func (d dynamicConfig) ProxySubscriptionAllowProxyFallback() bool {
	return Load().ProxySubscriptionAllowProxyFallback
}
func (d dynamicConfig) BackgroundImage() string { return Load().BackgroundImage }
func (d dynamicConfig) FontSize() string        { return Load().FontSize }
func (d dynamicConfig) FontColorType() string   { return Load().FontColorType }
func (d dynamicConfig) FontColor() string       { return Load().FontColor }
func (d dynamicConfig) CustomBgPresets() []string {
	c := Load()
	out := make([]string, len(c.CustomBgPresets))
	copy(out, c.CustomBgPresets)
	return out
}
func (d dynamicConfig) BaseModels() []string             { return Load().BaseModels() }
func (d dynamicConfig) AliasMap() map[string]string      { return Load().AliasMap() }
func (d dynamicConfig) ModelsWithFakeVariants() []string { return Load().ModelsWithFakeVariants() }
func (d dynamicConfig) FakePrefixes() []string           { return Load().FakePrefixes() }
func (d dynamicConfig) ResolveModelName(s string) string { return Load().ResolveModelName(s) }
func (d dynamicConfig) AutoRefreshLogs() bool            { return Load().GetAutoRefreshLogs() }
func (d dynamicConfig) ConfigDir() string                { return Load().ConfigDir() }
func (d dynamicConfig) ConfigPath() string               { return Load().ConfigPath() }
