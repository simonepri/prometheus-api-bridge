// Package main runs the Prometheus API bridge server.
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/simonepri/prometheus-api-bridge/bridge/api"
	"github.com/simonepri/prometheus-api-bridge/bridge/backend"
	"github.com/simonepri/prometheus-api-bridge/bridge/backend/signoz"
	"github.com/simonepri/prometheus-api-bridge/bridge/telemetry"
)

const (
	defaultAddress        = ":9090"
	defaultMaxHeaderBytes = 64 << 10
)

type runtimeConfig struct {
	address                 string
	backendType             string
	bearerToken             string
	timeout                 time.Duration
	discoveryLookback       time.Duration
	maxQueryRange           time.Duration
	maxConcurrentQueries    int
	maxPointsPerSeries      int
	maxRequestBodyBytes     int
	maxMatchersPerRequest   int
	maxSeries               int
	maxSamples              int
	maxBackendResponseBytes int
	telemetryEnabled        bool
	telemetryInterval       time.Duration
}

func main() {
	logger := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	if err := run(logger); err != nil {
		logger.Error("bridge stopped", "error", err)
		os.Exit(1)
	}
}

func run(logger *slog.Logger) error {
	config, err := runtimeConfigFromEnvironment()
	if err != nil {
		return err
	}
	client := backendHTTPClient(config.timeout)
	querier, err := backendFromEnvironment(
		config.backendType,
		client,
		int64(config.maxBackendResponseBytes),
		config.maxSeries,
		config.maxSamples,
	)
	if err != nil {
		return err
	}
	observer, err := telemetry.New(context.Background(), telemetry.Config{
		Enabled:        config.telemetryEnabled,
		Backend:        config.backendType,
		ExportInterval: config.telemetryInterval,
	})
	if err != nil {
		return fmt.Errorf("configure telemetry: %w", err)
	}
	httpServer := bridgeHTTPServer(config, querier, observer, logger)

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	errorsChannel := make(chan error, 1)
	go func() {
		logger.Info("starting Prometheus API bridge", "address", config.address, "backend", config.backendType)
		if serveErr := httpServer.ListenAndServe(); serveErr != nil && !errors.Is(serveErr, http.ErrServerClosed) {
			errorsChannel <- fmt.Errorf("serve HTTP: %w", serveErr)
		}
	}()
	var serveErr error
	select {
	case <-ctx.Done():
	case serveErr = <-errorsChannel:
	}
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	if err := httpServer.Shutdown(shutdownCtx); err != nil {
		logger.Error("HTTP shutdown failed", "error", err)
	}
	cancel()
	telemetryCtx, cancelTelemetry := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancelTelemetry()
	if err := observer.Shutdown(telemetryCtx); err != nil {
		logger.Error("telemetry shutdown failed", "error", err)
	}
	return serveErr
}

func runtimeConfigFromEnvironment() (runtimeConfig, error) {
	config := runtimeConfig{}
	var err error
	config.timeout, err = envDuration("BRIDGE_QUERY_TIMEOUT", 30*time.Second)
	if err != nil {
		return runtimeConfig{}, err
	}
	config.discoveryLookback, err = envDuration("BRIDGE_DISCOVERY_LOOKBACK", time.Hour)
	if err != nil {
		return runtimeConfig{}, err
	}
	config.maxQueryRange, err = envDuration("BRIDGE_MAX_QUERY_RANGE", 30*24*time.Hour)
	if err != nil {
		return runtimeConfig{}, err
	}
	config.maxConcurrentQueries, err = envPositiveInt("BRIDGE_MAX_CONCURRENT_QUERIES", 10)
	if err != nil {
		return runtimeConfig{}, err
	}
	config.maxPointsPerSeries, err = envPositiveInt("BRIDGE_MAX_POINTS_PER_SERIES", 50_000)
	if err != nil {
		return runtimeConfig{}, err
	}
	config.maxRequestBodyBytes, err = envPositiveInt("BRIDGE_MAX_REQUEST_BODY_BYTES", 1<<20)
	if err != nil {
		return runtimeConfig{}, err
	}
	config.maxMatchersPerRequest, err = envPositiveInt("BRIDGE_MAX_MATCHERS_PER_REQUEST", 32)
	if err != nil {
		return runtimeConfig{}, err
	}
	config.maxSeries, err = envPositiveInt("BRIDGE_MAX_SERIES", backend.DefaultMaxSeries)
	if err != nil {
		return runtimeConfig{}, err
	}
	config.maxSamples, err = envPositiveInt("BRIDGE_MAX_SAMPLES", backend.DefaultMaxSamples)
	if err != nil {
		return runtimeConfig{}, err
	}
	config.maxBackendResponseBytes, err = envPositiveInt(
		"BRIDGE_MAX_BACKEND_RESPONSE_BYTES",
		int(backend.DefaultMaxResponseBytes),
	)
	if err != nil {
		return runtimeConfig{}, err
	}
	config.telemetryEnabled, err = envBool("BRIDGE_TELEMETRY_ENABLED", false)
	if err != nil {
		return runtimeConfig{}, err
	}
	config.telemetryInterval, err = envDuration("BRIDGE_TELEMETRY_EXPORT_INTERVAL", 30*time.Second)
	if err != nil {
		return runtimeConfig{}, err
	}
	config.bearerToken, err = secretEnvironment("BRIDGE_BEARER_TOKEN", "BRIDGE_BEARER_TOKEN_FILE")
	if err != nil {
		return runtimeConfig{}, fmt.Errorf("configure API authentication: %w", err)
	}
	config.backendType = strings.ToLower(strings.TrimSpace(os.Getenv("BRIDGE_BACKEND")))
	config.address = envOrDefault("BRIDGE_LISTEN_ADDRESS", defaultAddress)
	return config, nil
}

func bridgeHTTPServer(
	config runtimeConfig,
	querier backend.Querier,
	observer api.Observer,
	logger *slog.Logger,
) *http.Server {
	return &http.Server{
		Addr: config.address,
		Handler: api.NewServerWithOptions(querier, logger, api.Options{
			Timeout:               config.timeout,
			DiscoveryLookback:     config.discoveryLookback,
			MaxQueryRange:         config.maxQueryRange,
			MaxConcurrentQueries:  config.maxConcurrentQueries,
			MaxPointsPerSeries:    config.maxPointsPerSeries,
			MaxRequestBodyBytes:   int64(config.maxRequestBodyBytes),
			MaxMatchersPerRequest: config.maxMatchersPerRequest,
			MaxSeries:             config.maxSeries,
			MaxSamples:            config.maxSamples,
			Observer:              observer,
			BearerToken:           config.bearerToken,
		}).Handler(),
		ReadTimeout:       10 * time.Second,
		ReadHeaderTimeout: 5 * time.Second,
		WriteTimeout:      config.timeout + 5*time.Second,
		IdleTimeout:       2 * time.Minute,
		MaxHeaderBytes:    defaultMaxHeaderBytes,
	}
}

func backendFromEnvironment(
	backendType string,
	httpClient *http.Client,
	maxResponseBytes int64,
	maxSeries int,
	maxSamples int,
) (backend.Querier, error) {
	switch backendType {
	case "signoz":
		apiKey, err := secretEnvironment("BRIDGE_SIGNOZ_API_KEY", "BRIDGE_SIGNOZ_API_KEY_FILE")
		if err != nil {
			return nil, fmt.Errorf("load SigNoz API key: %w", err)
		}
		return signoz.New(signoz.Config{
			URL:              os.Getenv("BRIDGE_SIGNOZ_URL"),
			APIKey:           apiKey,
			MaxResponseBytes: maxResponseBytes,
			MaxSeries:        maxSeries,
			MaxSamples:       maxSamples,
		}, httpClient)
	default:
		return nil, fmt.Errorf("BRIDGE_BACKEND must be signoz")
	}
}

func secretEnvironment(valueName string, fileName string) (string, error) {
	if rawValue, configured := os.LookupEnv(valueName); configured {
		value := strings.TrimSpace(rawValue)
		if value == "" {
			return "", fmt.Errorf("%s must not be empty when configured", valueName)
		}
		return value, nil
	}
	rawPath, configured := os.LookupEnv(fileName)
	if !configured {
		return "", nil
	}
	path := strings.TrimSpace(rawPath)
	if path == "" {
		return "", fmt.Errorf("%s must not be empty when configured", fileName)
	}
	// #nosec G703 -- the operator explicitly configures the mounted Secret path.
	content, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("read %s: %w", fileName, err)
	}
	value := strings.TrimSpace(string(content))
	if value == "" {
		return "", fmt.Errorf("%s points to an empty secret", fileName)
	}
	return value, nil
}

func backendHTTPClient(timeout time.Duration) *http.Client {
	return &http.Client{
		Timeout:       timeout,
		CheckRedirect: backend.RejectRedirect,
	}
}

func envOrDefault(name string, fallback string) string {
	if value := strings.TrimSpace(os.Getenv(name)); value != "" {
		return value
	}
	return fallback
}

func envDuration(name string, fallback time.Duration) (time.Duration, error) {
	value := strings.TrimSpace(os.Getenv(name))
	if value == "" {
		return fallback, nil
	}
	parsed, err := time.ParseDuration(value)
	if err != nil || parsed <= 0 {
		return 0, fmt.Errorf("%s must be a positive duration", name)
	}
	return parsed, nil
}

func envPositiveInt(name string, fallback int) (int, error) {
	value := strings.TrimSpace(os.Getenv(name))
	if value == "" {
		return fallback, nil
	}
	parsed, err := strconv.Atoi(value)
	if err != nil || parsed <= 0 {
		return 0, fmt.Errorf("%s must be a positive integer", name)
	}
	return parsed, nil
}

func envBool(name string, fallback bool) (bool, error) {
	value := strings.TrimSpace(os.Getenv(name))
	if value == "" {
		return fallback, nil
	}
	parsed, err := strconv.ParseBool(value)
	if err != nil {
		return false, fmt.Errorf("%s must be a boolean", name)
	}
	return parsed, nil
}
