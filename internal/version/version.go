package version

// Version and Commit are set at build time via:
//
//	go build -ldflags "-X ...version.VERSION=0.4.0 -X ...version.Commit=abc123"
var (
	VERSION = "dev"
	Commit  = "dev"
)
