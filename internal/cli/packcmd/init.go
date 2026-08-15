package packcmd

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/spf13/cobra"
)

var nameRe = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]*[a-z0-9])?$`)

// NewInitCmd 构造 `mechpack init`。
func NewInitCmd() *cobra.Command {
	var dir string
	var force bool

	cmd := &cobra.Command{
		Use:   "init <name>",
		Short: "Generate a Pack skeleton that lints cleanly out of the box",
		Long: `Generate a Pack skeleton.

The generated skeleton is L1 shape — single role, single profile, systemd
runtime, and doesn't touch profiles / placement / cardinality / linkInto /
multi-disk. Add those capabilities incrementally, per spec, when you need
them.`,
		Args: cobra.ExactArgs(1),
		RunE: func(c *cobra.Command, args []string) error {
			name := args[0]
			if !nameRe.MatchString(name) || len(name) > 63 {
				return fmt.Errorf("name %q doesn't follow DNS label rules\n"+
					"  only lowercase letters, digits, and hyphens; can't start or end with a hyphen; max 63 chars", name)
			}
			target := dir
			if target == "" {
				target = name
			}
			return scaffold(c, target, name, force)
		},
	}

	cmd.Flags().StringVar(&dir, "dir", "", "Output directory (defaults to the same name as name)")
	cmd.Flags().BoolVar(&force, "force", false, "Allow writing into an existing non-empty directory")
	return cmd
}

func scaffold(c *cobra.Command, dir, name string, force bool) error {
	if entries, err := os.ReadDir(dir); err == nil && len(entries) > 0 && !force {
		return fmt.Errorf("directory %s is not empty; use --force to overwrite", dir)
	}

	files := map[string]string{
		"pack.yaml":                        manifestTemplate(name),
		"templates/" + name + ".conf.tmpl": confTemplate(name),
		"README.md":                        readmeTemplate(name),
		".gitignore":                       "dist/\n*.mpack\n*.sig\n",
	}

	for rel, content := range files {
		abs := filepath.Join(dir, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
			return err
		}
		if err := os.WriteFile(abs, []byte(content), 0o644); err != nil {
			return err
		}
	}
	// hooks/ 与 files/ 留空目录，让作者一眼看到它们的存在
	for _, sub := range []string{"hooks", "files"} {
		if err := os.MkdirAll(filepath.Join(dir, sub), 0o755); err != nil {
			return err
		}
	}

	out := c.OutOrStdout()
	fmt.Fprintf(out, "Created Pack skeleton: %s\n\n", dir)
	for _, rel := range []string{"pack.yaml", "templates/" + name + ".conf.tmpl", "README.md"} {
		fmt.Fprintf(out, "  %s\n", rel)
	}
	fmt.Fprintf(out, "  hooks/            (escape-hatch scripts, can be left empty)\n")
	fmt.Fprintf(out, "  files/            (static files, can be left empty)\n")
	fmt.Fprintf(out, `
Next:
  1. Build the artifacts with your own toolchain (make / mvn / go build / download an upstream tarball)
  2. Point to them in pack.yaml's sources section
  3. mechpack assemble %s
`, dir)
	return nil
}

func manifestTemplate(name string) string {
	return strings.ReplaceAll(`schema: pack/v1
name: NAME
version: "0.1.0"
description: "TODO: one sentence describing what this component is"
license: Apache-2.0

# platforms can be omitted -- assemble derives it from sources' platform
# keys and writes it into the release artifact. Every blob's platform keys
# must match, or assemble will error and ask you to fill it in or declare
# it explicitly. A host-config-only Pack (no payload) must declare it
# explicitly.
# platforms: [linux/amd64, linux/arm64]

# sources describes where the payload comes from. It **does not end up in
# the release artifact** -- assemble uses it to compute sha256/size/filename
# and turns it into the blobs section.
#
# Paths are relative to this directory; use --source-root to point at a
# different base directory when build artifacts live elsewhere.
sources:
  main:
    linux/amd64: dist/NAME-linux-amd64.tar.gz
    linux/arm64: dist/NAME-linux-arm64.tar.gz

params:
  port:
    type: port
    default: 8080
    description: "Listen port"
    restartRequired: true

  log_level:
    type: enum
    values: [debug, info, warn, error]
    default: info
    reloadRequired: true

roles:
  # A single-role Pack can omit name, which defaults to "default"
  - resources:
      - user:    { name: NAME, system: true }
      - archive: { blob: main, dest: "{{ .Paths.Generation }}", strip: 1 }
      - template:
          src:    NAME.conf.tmpl
          dest:   "{{ .Paths.Config }}/NAME.conf"
          owner:  NAME
          mode:   "0640"
          notify: reload

    workload:
      runtime: systemd
      systemd:
        exec:       "{{ .Paths.Current }}/bin/NAME --config {{ .Paths.Config }}/NAME.conf"
        # Parameters can only use reloadRequired once execReload is declared
        execReload: "/bin/kill -HUP $MAINPID"
        user:       NAME
        restart:    always

    health:
      http: { path: /healthz, port: "{{ .Params.port }}" }
      startupGrace: 15s
`, "NAME", name)
}

func confTemplate(name string) string {
	return strings.ReplaceAll(`# Rendered by Mecharion, do not edit by hand
# Component: {{ .Component }}  Generation: {{ .Generation.Seq }}

listen = 0.0.0.0:{{ .Params.port }}
log_level = {{ .Params.log_level }}

data_dir = {{ .Paths.Data }}
log_dir  = {{ .Paths.Logs }}
`, "NAME", name)
}

func readmeTemplate(name string) string {
	return strings.ReplaceAll("# NAME\n\n"+
		"TODO: what this component is, what it looks like once deployed.\n\n"+
		"## Build and package\n\n"+
		"```bash\n"+
		"# 1. Produce the binary with your own toolchain (mechpack doesn't do this step)\n"+
		"make dist\n\n"+
		"# 2. Assemble the Pack\n"+
		"mechpack assemble .\n\n"+
		"# 3. Validate\n"+
		"mechpack lint --hermetic dist/NAME-0.1.0-1\n"+
		"```\n\n"+
		"## Parameters\n\n"+
		"| Parameter | Type | Default | Description |\n"+
		"|---|---|---|---|\n"+
		"| `port` | port | 8080 | Listen port, requires a restart to change |\n"+
		"| `log_level` | enum | info | Log level, only requires a reload to change |\n",
		"NAME", name)
}
