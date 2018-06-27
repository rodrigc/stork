package storkctl

import (
	"github.com/spf13/cobra"
)

func newDescribeCommand() *cobra.Command {
	return &cobra.Command{
		Use:   "describe",
		Short: "Describe stork resources",
	}
}
