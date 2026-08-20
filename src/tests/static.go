// Package tests owns the executable chart validation contract.
package tests

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"unicode"
)

const kubernetesSchemaVersion = "1.36.3"

// Static validates the chart schema, render variants, and release package.
func Static(ctx context.Context, chartDir string) error {
	if err := requireTools("helm", "kubeconform"); err != nil {
		return err
	}
	repoDir := RepoDir()
	outputDir, packageDir, err := prepareStaticDirs(repoDir)
	if err != nil {
		return err
	}
	r := runner{dir: repoDir, out: os.Stdout}
	if err := validateChartSource(chartDir); err != nil {
		return err
	}
	if err := prepareChart(ctx, r, chartDir); err != nil {
		return err
	}
	if err := renderVariants(ctx, r, chartDir, outputDir); err != nil {
		return err
	}
	if err := validateRenderedContracts(ctx, r, chartDir, outputDir); err != nil {
		return err
	}
	if err := packageChart(ctx, r, chartDir, packageDir); err != nil {
		return err
	}

	fmt.Println("PASS: chart conventions, source renders, schemas, and package contents")
	return nil
}

type renderVariant struct {
	name string
	args []string
}

func prepareStaticDirs(repoDir string) (string, string, error) {
	outputDir := filepath.Join(repoDir, ".local", "rendered")
	packageDir := filepath.Join(repoDir, ".local", "package")
	for _, dir := range []string{outputDir, packageDir} {
		if err := os.MkdirAll(dir, 0o750); err != nil {
			return "", "", fmt.Errorf("create %s: %w", dir, err)
		}
	}
	return outputDir, packageDir, nil
}

func validateChartSource(chartDir string) error {
	valuesSchema, err := os.ReadFile(filepath.Join(chartDir, "values.schema.json"))
	if err != nil {
		return fmt.Errorf("read values schema: %w", err)
	}
	var schema any
	if err := json.Unmarshal(valuesSchema, &schema); err != nil {
		return fmt.Errorf("parse values schema: %w", err)
	}
	return validateTemplateConventions(chartDir)
}

func prepareChart(ctx context.Context, r runner, chartDir string) error {
	if _, err := r.run(ctx, "helm", "repo", "add", "prometheus-community", "https://prometheus-community.github.io/helm-charts", "--force-update"); err != nil {
		return err
	}
	if _, err := r.run(ctx, "helm", "dependency", "build", chartDir); err != nil {
		return err
	}
	_, err := r.run(ctx, "helm", "lint", chartDir, "--set-string", "clusterName=ci")
	return err
}

func renderVariants(ctx context.Context, r runner, chartDir string, outputDir string) error {
	for _, render := range chartRenderVariants() {
		if err := renderAndValidate(ctx, r, chartDir, outputDir, render.name, render.args...); err != nil {
			return err
		}
	}
	return nil
}

func chartRenderVariants() []renderVariant {
	return []renderVariant{
		{name: "standalone-node-sources", args: []string{
			"--set", "collection.mode=standalone",
			"--set", "collection.sources.cadvisor.enabled=true",
			"--set", "collection.sources.kubelet.enabled=true",
			"--set", "server.telemetry.enabled=true",
			"--set-string", "server.telemetry.endpoint=http://otel-gateway:4318",
		}},
		{name: "standalone-kubernetes-state", args: []string{
			"--set", "collection.mode=standalone",
			"--set", "collection.sources.kubeStateMetrics.enabled=true",
			"--set", "kube-state-metrics.enabled=true",
		}},
		{name: "standalone-extra-scrape", args: []string{
			"--set", "collection.mode=standalone",
			"--set-string", "collection.extraScrapeConfigs[0].job_name=application",
			"--set-string", "collection.extraScrapeConfigs[0].static_configs[0].targets[0]=application:9090",
		}},
		{name: "standalone-ray-http-sd", args: []string{
			"--set", "collection.mode=standalone",
			"--set-string", "collection.extraScrapeConfigs[0].job_name=ray",
			"--set-string", "collection.extraScrapeConfigs[0].http_sd_configs[0].url=http://ray-head.ray.svc:8265/api/prometheus/sd",
			"--set-string", "collection.extraScrapeConfigs[0].http_sd_configs[0].refresh_interval=30s",
		}},
		{name: "consumer-discovery", args: []string{"--set-string", "service.labels.headlamp-prometheus=true"}},
		{name: "network-policy", args: []string{
			"--set", "networkPolicy.enabled=true",
			"--set-string", "networkPolicy.ingress[0].from[0].namespaceSelector.matchLabels.kubernetes\\.io/metadata\\.name=consumers",
		}},
		{name: "existing", args: []string{
			"--set", "collection.mode=existing",
			"--set-string", "collection.existing.exporter=otlp_http/platform",
			"--set-string", "collection.existing.serviceAccount.name=otel-gateway",
			"--set-string", "collection.existing.serviceAccount.namespace=observability",
			"--set", "collection.sources.cadvisor.enabled=true",
		}},
	}
}

func validateRenderedContracts(
	ctx context.Context,
	r runner,
	chartDir string,
	outputDir string,
) error {
	checks := []func() error{
		func() error { return validateConsumerRender(outputDir) },
		func() error { return validateNetworkPolicyRender(outputDir) },
		func() error { return validateKubernetesStateRender(outputDir) },
		func() error { return validateExtraScrapeRender(outputDir) },
		func() error { return validateRayHTTPServiceDiscoveryRender(outputDir) },
		func() error { return validateExistingRender(outputDir) },
		func() error { return validateDisabledRender(ctx, r, chartDir) },
		func() error { return validateAuthenticationRender(ctx, r, chartDir) },
	}
	for _, check := range checks {
		if err := check(); err != nil {
			return err
		}
	}
	return nil
}

func readRender(outputDir string, name string) (string, error) {
	path := filepath.Join(outputDir, name+".yaml")
	content, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("read %s render: %w", name, err)
	}
	return string(content), nil
}

func validateConsumerRender(outputDir string) error {
	render, err := readRender(outputDir, "consumer-discovery")
	if err != nil {
		return err
	}
	if !strings.Contains(render, `headlamp-prometheus: "true"`) {
		return fmt.Errorf("generic service labels were not rendered")
	}
	if strings.Contains(render, "BRIDGE_PROFILES") || strings.Contains(render, "keda-grpc") {
		return fmt.Errorf("generic bridge render retained consumer-specific runtime configuration")
	}
	if !strings.Contains(render, "kind: PodDisruptionBudget") || !strings.Contains(render, "replicas: 2") {
		return fmt.Errorf("default render omitted production availability settings")
	}
	if !strings.Contains(render, "value: \"1048576\"") || !strings.Contains(render, "value: \"4194304\"") {
		return fmt.Errorf("integer environment settings were not rendered in decimal form")
	}
	if !strings.Contains(render, "name: BRIDGE_BEARER_TOKEN") ||
		!strings.Contains(render, `name: "prometheus-api-bridge-auth"`) {
		return fmt.Errorf("default render omitted Prometheus API authentication")
	}
	return nil
}

func validateNetworkPolicyRender(outputDir string) error {
	render, err := readRender(outputDir, "network-policy")
	if err != nil {
		return err
	}
	if !strings.Contains(render, "kind: NetworkPolicy") ||
		!strings.Contains(render, "kubernetes.io/metadata.name: consumers") {
		return fmt.Errorf("network policy did not render its configured ingress selector")
	}
	return nil
}

func validateKubernetesStateRender(outputDir string) error {
	render, err := readRender(outputDir, "standalone-kubernetes-state")
	if err != nil {
		return err
	}
	if !strings.Contains(render, "standalone-kubernetes-state-kube-state-metrics.observability.svc:8080") {
		return fmt.Errorf("bundled kube-state-metrics target is not release-aware")
	}
	return nil
}

func validateExtraScrapeRender(outputDir string) error {
	render, err := readRender(outputDir, "standalone-extra-scrape")
	if err != nil {
		return err
	}
	if !strings.Contains(render, "job_name: application") || !strings.Contains(render, "application:9090") {
		return fmt.Errorf("extra Prometheus receiver scrape configuration was not rendered")
	}
	return nil
}

func validateRayHTTPServiceDiscoveryRender(outputDir string) error {
	render, err := readRender(outputDir, "standalone-ray-http-sd")
	if err != nil {
		return err
	}
	if !strings.Contains(render, "job_name: ray") ||
		!strings.Contains(render, "http_sd_configs:") ||
		!strings.Contains(render, "http://ray-head.ray.svc:8265/api/prometheus/sd") {
		return fmt.Errorf("ray HTTP service discovery scrape configuration was not rendered")
	}
	return nil
}

func validateExistingRender(outputDir string) error {
	render, err := readRender(outputDir, "existing")
	if err != nil {
		return err
	}
	if strings.Contains(render, "kind: Deployment\nmetadata:\n  name: existing-prometheus-api-bridge-collector") {
		return fmt.Errorf("existing mode rendered a chart-owned Collector Deployment")
	}
	if !strings.Contains(render, "name: otel-gateway") || !strings.Contains(render, "collector.yaml: |") {
		return fmt.Errorf("existing mode omitted its RBAC subject or Collector configuration")
	}
	if !strings.Contains(render, `exporters: ["otlp_http/platform"]`) || strings.Contains(render, "\n    exporters:\n") {
		return fmt.Errorf("existing mode did not reuse only its declared Collector exporter")
	}
	return nil
}

func validateDisabledRender(ctx context.Context, r runner, chartDir string) error {
	render, err := r.quietHelm(
		ctx,
		"template", "disabled", chartDir,
		"--set-string", "clusterName=ci",
	)
	if err != nil {
		return fmt.Errorf("render backend-only installation: %w", err)
	}
	if strings.Contains(render, "app.kubernetes.io/component: collector") {
		return fmt.Errorf("backend-only installation rendered Collector resources")
	}
	return nil
}

func validateAuthenticationRender(ctx context.Context, r runner, chartDir string) error {
	_, err := r.quietHelm(
		ctx,
		"template", "unsafe", chartDir,
		"--set-string", "clusterName=ci",
		"--set-string", "server.auth.bearerTokenSecret.name=",
	)
	if err == nil {
		return fmt.Errorf("chart accepted missing bearer authentication without explicit acknowledgement")
	}
	_, err = r.quietHelm(
		ctx,
		"template", "acknowledged", chartDir,
		"--set-string", "clusterName=ci",
		"--set", "server.auth.allowUnauthenticated=true",
		"--set-string", "server.auth.bearerTokenSecret.name=",
	)
	if err != nil {
		return fmt.Errorf("render explicitly acknowledged unauthenticated installation: %w", err)
	}
	return nil
}

func packageChart(ctx context.Context, r runner, chartDir string, packageDir string) error {
	version, err := chartVersion(filepath.Join(chartDir, "Chart.yaml"))
	if err != nil {
		return err
	}
	if _, err := r.run(ctx, "helm", "package", chartDir, "--destination", packageDir); err != nil {
		return err
	}
	return validatePackage(filepath.Join(packageDir, "prometheus-api-bridge-"+version+".tgz"))
}

func validateTemplateConventions(chartDir string) error {
	templatesDir := filepath.Join(chartDir, "templates")
	return filepath.WalkDir(templatesDir, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() || filepath.Ext(path) != ".yaml" {
			return nil
		}

		name := strings.TrimSuffix(entry.Name(), ".yaml")
		if name != strings.ToLower(name) || strings.Contains(name, "_") {
			return fmt.Errorf("template filename must use dashed notation: %s", path)
		}
		// #nosec G122 -- the path comes from a walk of the repository-owned chart tree.
		content, err := os.ReadFile(path)
		if err != nil {
			return fmt.Errorf("read template %s: %w", path, err)
		}
		if strings.Contains(string(content), "\n---") {
			return fmt.Errorf("template must contain one resource definition: %s", path)
		}

		kinds := resourceKinds(string(content))
		if len(kinds) != 1 {
			return fmt.Errorf("template must contain exactly one resource kind: %s", path)
		}
		kindName := dashedKind(kinds[0])
		if !strings.Contains(name, kindName) {
			return fmt.Errorf("template filename %q must reflect resource kind %q", entry.Name(), kinds[0])
		}
		return nil
	})
}

func resourceKinds(content string) []string {
	var kinds []string
	for line := range strings.SplitSeq(content, "\n") {
		if kind, ok := strings.CutPrefix(line, "kind:"); ok {
			kinds = append(kinds, strings.TrimSpace(kind))
		}
	}
	return kinds
}

func dashedKind(kind string) string {
	var result strings.Builder
	for index, character := range kind {
		if index > 0 && unicode.IsUpper(character) {
			result.WriteByte('-')
		}
		result.WriteRune(unicode.ToLower(character))
	}
	return result.String()
}

func renderAndValidate(
	ctx context.Context,
	r runner,
	chartDir string,
	outputDir string,
	name string,
	extraArgs ...string,
) error {
	args := []string{
		"template", name, chartDir,
		"--namespace", "observability",
		"--set-string", "clusterName=ci",
	}
	args = append(args, extraArgs...)
	output, err := r.quietHelm(ctx, args...)
	if err != nil {
		return err
	}
	path := filepath.Join(outputDir, name+".yaml")
	if err := os.WriteFile(path, []byte(output+"\n"), 0o600); err != nil {
		return fmt.Errorf("write rendered chart %s: %w", name, err)
	}
	_, err = r.run(
		ctx,
		"kubeconform",
		"-kubernetes-version", kubernetesSchemaVersion,
		"-strict",
		"-summary",
		path,
	)
	return err
}

func chartVersion(path string) (string, error) {
	return chartMetadataValue(path, "version")
}

func chartMetadataValue(path string, field string) (string, error) {
	content, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("read Chart.yaml: %w", err)
	}
	for _, line := range strings.Split(string(content), "\n") {
		if value, ok := strings.CutPrefix(strings.TrimSpace(line), field+":"); ok {
			version := strings.TrimSpace(value)
			version = strings.Trim(version, `"'`)
			if version != "" {
				return version, nil
			}
		}
	}
	return "", fmt.Errorf("chart metadata has no %s", field)
}

func validatePackage(path string) error {
	file, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("open packaged chart: %w", err)
	}
	defer file.Close()
	gzipReader, err := gzip.NewReader(file)
	if err != nil {
		return fmt.Errorf("read packaged chart gzip: %w", err)
	}
	defer gzipReader.Close()
	tarReader := tar.NewReader(gzipReader)
	for {
		header, err := tarReader.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return fmt.Errorf("read packaged chart: %w", err)
		}
		name := header.Name
		if strings.Contains(name, "/test/") ||
			strings.HasSuffix(name, ".go") ||
			strings.HasSuffix(name, "/go.mod") ||
			strings.HasSuffix(name, "/go.sum") {
			return fmt.Errorf("packaged chart contains non-chart source: %s", name)
		}
	}
	return nil
}
