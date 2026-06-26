// Command medea is the Medea control-plane binary: `medea serve` runs the gRPC
// server; the other verbs are clients against it. See design/api-and-auth.md.
package main

import (
	"fmt"
	"os"

	"github.com/spf13/cobra"
)

var rootCmd = &cobra.Command{
	Use:          "medea",
	Short:        "Medea — external control plane for Talos clusters",
	SilenceUsage: true,
}

func main() {
	if err := rootCmd.Execute(); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}
