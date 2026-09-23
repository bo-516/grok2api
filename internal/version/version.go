// Package version holds the agent-mock release string shared by -version and startup logs.
package version

// Version is the agent-mock release printed by -version and the startup banner.
// It is not the grok CLI version. Shipping a mismatch here makes support reports
// point at the wrong binary.
const Version = "0.1.0"
