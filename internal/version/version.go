package version

import "runtime"

var (
	Version = "dev"
	Commit  = "none"
	Date    = "unknown"
)

func GoVersion() string { return runtime.Version() }
