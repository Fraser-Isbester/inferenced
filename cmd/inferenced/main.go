// Command inferenced is the local LLM inference gateway CLI. It meters, enforces,
// and observes inference traffic for the registered providers. The command tree
// lives in internal/cli; this entry point only wires in the build version.
package main

import (
	"os"

	"github.com/Fraser-Isbester/inferenced/internal/cli"
)

// version is the build version, injected via -ldflags "-X main.version=..." by
// goreleaser. It defaults to "dev" for local builds.
var version = "dev"

func main() {
	if err := cli.Execute(version); err != nil {
		os.Exit(1)
	}
}
