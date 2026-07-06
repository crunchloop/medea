// Command medea is the Medea control-plane binary: `medea serve` runs the gRPC
// server; the other verbs are clients against it. See design/api-and-auth.md.
package main

import (
	"fmt"
	"os"

	"github.com/spf13/cobra"
)

// version is stamped at build time by GoReleaser (-ldflags "-X main.version=...").
// Defaults to "dev" for `go build`/`go install` from source.
var version = "dev"

var rootCmd = &cobra.Command{
	Use:          "medea",
	Short:        "Medea — external control plane for Talos clusters",
	Version:      version,
	SilenceUsage: true,
}

func main() {
	if err := rootCmd.Execute(); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}
