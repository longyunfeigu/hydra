package cmd

import (
	"fmt"
	"os"

	"github.com/spf13/cobra"
)

var rootCmd = &cobra.Command{
	Use:   "hydra",
	Short: "Multi-model adversarial code review tool",
	Long: `Hydra uses multiple AI models to independently review code changes,
then facilitates a structured debate to produce comprehensive review results.`,
}

func Execute() {
	if err := rootCmd.Execute(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func init() {
	rootCmd.AddCommand(reviewCmd)
	rootCmd.AddCommand(initCmd)
}
