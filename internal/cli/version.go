package cli

import (
	"encoding/json"
	"fmt"

	"github.com/spf13/cobra"
	"gopkg.in/yaml.v3"

	"github.com/mecharion/mecharion/internal/version"
)

func newVersionCmd(component string, gf *GlobalFlags) *cobra.Command {
	var short bool

	cmd := &cobra.Command{
		Use:   "version",
		Short: "Print version and build info",
		Args:  cobra.NoArgs,
		RunE: func(c *cobra.Command, _ []string) error {
			info := version.Get(component)
			out := c.OutOrStdout()

			if short {
				fmt.Fprintln(out, info.Version)
				return nil
			}

			switch gf.Output {
			case OutputJSON:
				enc := json.NewEncoder(out)
				enc.SetIndent("", "  ")
				return enc.Encode(info)
			case OutputYAML:
				return yaml.NewEncoder(out).Encode(info)
			default:
				fmt.Fprintln(out, info.String())
				return nil
			}
		},
	}

	cmd.Flags().BoolVar(&short, "short", false, "Print only the version number")
	return cmd
}
