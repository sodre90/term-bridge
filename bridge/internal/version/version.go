package version

// v is the bridge version. It can be overridden at build time via
// -ldflags "-X github.com/sodre90/term-bridge/internal/version.v=x.y.z".
var v = "0.9.0-dev"

// String returns the bridge version string.
func String() string { return v }
