package packcmd

import (
	"fmt"

	"github.com/spf13/cobra"

	"github.com/mecharion/mecharion/internal/pack"
)

// NewAssembleCmd 构造 `mechpack assemble`。
func NewAssembleCmd(output *string) *cobra.Command {
	var opts pack.AssembleOptions

	cmd := &cobra.Command{
		Use:   "assemble [dir]",
		Short: "Assemble a source Pack directory into a publishable Pack",
		Long: `Collect local artifacts per pack.yaml's sources section, compute sha256, and
produce a publishable Pack.

assemble doesn't build your software — that's done by your own toolchain
(make / mvn / go build / downloading an upstream tarball), assemble only
does the assembly.

It will:
  · compute sha256 / size / filename for each payload per sources
  · derive platforms from each blob's platform keys when not declared (errors if inconsistent)
  · replace the sources section with a blobs section, keeping pack.yaml's comments and field order intact
  · copy templates/ files/ hooks/ and payloads (content-addressed naming, automatic dedup)
  · run a full lint on the artifact`,
		Args: cobra.MaximumNArgs(1),
		RunE: func(c *cobra.Command, args []string) error {
			dir := "."
			if len(args) == 1 {
				dir = args[0]
			}
			res, err := pack.Assemble(dir, opts)
			if err != nil {
				return err
			}

			if *output == OutputJSON || *output == OutputYAML {
				if err := encode(c.OutOrStdout(), *output, res); err != nil {
					return err
				}
			} else {
				printAssemble(c, res)
			}

			if res.Lint != nil && !res.Lint.OK() {
				return &exitError{
					code: ExitValidation,
					msg:  "artifact failed validation (see details with mechpack lint)",
				}
			}
			return nil
		},
	}

	f := cmd.Flags()
	f.StringVar(&opts.Out, "out", "", "Output directory (default dist/<name>-<version>-<revision>)")
	f.StringVar(&opts.SourceRoot, "source-root", "",
		"Base directory for relative paths in sources (defaults to the Pack directory)")
	f.BoolVar(&opts.Force, "force", false, "Overwrite an existing non-empty output directory")
	f.BoolVar(&opts.SkipLint, "skip-lint", false, "Skip artifact validation (troubleshooting only)")
	return cmd
}

func printAssemble(c *cobra.Command, res *pack.AssembleResult) {
	out := c.OutOrStdout()

	fmt.Fprintf(out, "%s %s-%d  →  %s\n", res.Name, res.Version, res.Revision, res.Out)
	fmt.Fprintf(out, "Platforms: %v\n", res.Platforms)

	if len(res.Blobs) > 0 {
		fmt.Fprintf(out, "\nBlobs (%d):\n", len(res.Blobs))
		for _, b := range res.Blobs {
			mark := ""
			if b.Reused {
				mark = "  (deduped)"
			}
			fmt.Fprintf(out, "  %-10s %-14s %10s  %s\n    sha256:%s%s\n",
				b.Name, b.Platform, humanSize(b.Size), b.Filename, b.SHA256[:16]+"…", mark)
		}
		fmt.Fprintf(out, "\nTotal %s\n", humanSize(res.TotalSize))
	}

	if res.Lint != nil {
		errs, warns := len(res.Lint.Errors()), len(res.Lint.Warnings())
		switch {
		case errs > 0:
			fmt.Fprintf(out, "\n✗ artifact validation failed: %d error(s), %d warning(s)\n", errs, warns)
			for _, f := range res.Lint.Findings {
				fmt.Fprintf(out, "  %s\n", f.String())
			}
		case warns > 0:
			fmt.Fprintf(out, "\n! artifact validation passed, %d warning(s)\n", warns)
			for _, f := range res.Lint.Warnings() {
				fmt.Fprintf(out, "  %s\n", f.String())
			}
		default:
			fmt.Fprintln(out, "\n✓ artifact validation passed")
		}
	}
}
