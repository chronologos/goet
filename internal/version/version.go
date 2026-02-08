package version

// Commit is the git commit hash, set at build time via:
//
//	go build -ldflags "-X github.com/chronologos/goet/internal/version.Commit=abc123"
var Commit = "dev"
