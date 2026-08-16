package tests

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
)

type runner struct {
	dir string
	env map[string]string
	out io.Writer
}

// ChartDir returns the chart directory from source and test working directories.
func ChartDir() string {
	dir, err := os.Getwd()
	if err != nil {
		panic(err)
	}
	for {
		chartDir := filepath.Join(dir, "chart")
		if _, err := os.Stat(filepath.Join(chartDir, "Chart.yaml")); err == nil {
			return chartDir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			panic("could not find chart/Chart.yaml")
		}
		dir = parent
	}
}

// RepoDir returns the repository root containing src/chart.
func RepoDir() string {
	return filepath.Dir(filepath.Dir(ChartDir()))
}

func (r runner) command(ctx context.Context, stdin io.Reader, stream bool, name string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Dir = r.dir
	cmd.Env = mergedEnv(r.env)
	cmd.Stdin = stdin

	var output bytes.Buffer
	cmd.Stdout = &output
	cmd.Stderr = &output
	if stream {
		cmd.Stdout = io.MultiWriter(r.out, &output)
		cmd.Stderr = io.MultiWriter(r.out, &output)
	}
	if err := cmd.Run(); err != nil {
		return strings.TrimSpace(output.String()), fmt.Errorf(
			"run %s %s: %w\n%s",
			name,
			strings.Join(args, " "),
			err,
			strings.TrimSpace(output.String()),
		)
	}
	return strings.TrimSpace(output.String()), nil
}

func (r runner) run(ctx context.Context, name string, args ...string) (string, error) {
	return r.command(ctx, nil, true, name, args...)
}

func (r runner) quietHelm(ctx context.Context, args ...string) (string, error) {
	return r.command(ctx, nil, false, "helm", args...)
}

func mergedEnv(overrides map[string]string) []string {
	env := make(map[string]string)
	for _, item := range os.Environ() {
		key, value, ok := strings.Cut(item, "=")
		if ok {
			env[key] = value
		}
	}
	for key, value := range overrides {
		env[key] = value
	}
	keys := make([]string, 0, len(env))
	for key := range env {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	result := make([]string, 0, len(keys))
	for _, key := range keys {
		result = append(result, key+"="+env[key])
	}
	return result
}

func requireTools(names ...string) error {
	for _, name := range names {
		if _, err := exec.LookPath(name); err != nil {
			return fmt.Errorf("missing required tool %s: %w", name, err)
		}
	}
	return nil
}
