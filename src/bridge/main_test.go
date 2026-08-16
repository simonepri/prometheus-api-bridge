package main

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestEnvBool(t *testing.T) {
	t.Setenv("BRIDGE_TEST_BOOL", "true")
	value, err := envBool("BRIDGE_TEST_BOOL", false)
	if err != nil || !value {
		t.Fatalf("value = %t, error = %v", value, err)
	}
	t.Setenv("BRIDGE_TEST_BOOL", "sometimes")
	if _, err := envBool("BRIDGE_TEST_BOOL", false); err == nil {
		t.Fatal("invalid boolean was accepted")
	}
}

func TestSecretEnvironment(t *testing.T) {
	tests := []struct {
		name           string
		valueSet       bool
		value          string
		fileSet        bool
		filePath       string
		fileContents   *string
		want           string
		wantErrorMatch string
	}{
		{name: "absent optional secret"},
		{name: "direct value", valueSet: true, value: " direct-token\n", want: "direct-token"},
		{name: "direct value wins", valueSet: true, value: "direct-token", fileSet: true, filePath: "/missing", want: "direct-token"},
		{name: "empty direct value", valueSet: true, value: " \n", wantErrorMatch: "TEST_SECRET"},
		{name: "empty file path", fileSet: true, filePath: " ", wantErrorMatch: "TEST_SECRET_FILE"},
		{name: "unreadable file", fileSet: true, filePath: "/does/not/exist", wantErrorMatch: "read TEST_SECRET_FILE"},
		{name: "empty file", fileSet: true, fileContents: stringPointer(""), wantErrorMatch: "empty secret"},
		{name: "whitespace file", fileSet: true, fileContents: stringPointer(" \n"), wantErrorMatch: "empty secret"},
		{name: "file value", fileSet: true, fileContents: stringPointer(" file-token\n"), want: "file-token"},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			unsetEnvironment(t, "TEST_SECRET")
			unsetEnvironment(t, "TEST_SECRET_FILE")
			if test.valueSet {
				t.Setenv("TEST_SECRET", test.value)
			}
			if test.fileSet {
				path := test.filePath
				if test.fileContents != nil {
					path = filepath.Join(t.TempDir(), "secret")
					if err := os.WriteFile(path, []byte(*test.fileContents), 0o600); err != nil {
						t.Fatalf("write secret fixture: %v", err)
					}
				}
				t.Setenv("TEST_SECRET_FILE", path)
			}

			value, err := secretEnvironment("TEST_SECRET", "TEST_SECRET_FILE")
			if test.wantErrorMatch == "" && err != nil {
				t.Fatalf("secretEnvironment() error = %v", err)
			}
			if test.wantErrorMatch != "" && (err == nil || !strings.Contains(err.Error(), test.wantErrorMatch)) {
				t.Fatalf("secretEnvironment() error = %v, want match %q", err, test.wantErrorMatch)
			}
			if value != test.want {
				t.Fatalf("secretEnvironment() = %q, want %q", value, test.want)
			}
		})
	}
}

func TestRuntimeConfigLoadsBearerToken(t *testing.T) {
	unsetEnvironment(t, "BRIDGE_BEARER_TOKEN_FILE")
	t.Setenv("BRIDGE_BEARER_TOKEN", "bridge-token")

	config, err := runtimeConfigFromEnvironment()
	if err != nil {
		t.Fatalf("runtimeConfigFromEnvironment() error = %v", err)
	}
	if config.bearerToken != "bridge-token" {
		t.Fatalf("bearerToken = %q, want bridge-token", config.bearerToken)
	}
}

func TestRuntimeConfigRejectsUnreadableBearerTokenFile(t *testing.T) {
	unsetEnvironment(t, "BRIDGE_BEARER_TOKEN")
	t.Setenv("BRIDGE_BEARER_TOKEN_FILE", "/does/not/exist")

	_, err := runtimeConfigFromEnvironment()
	if err == nil || !strings.Contains(err.Error(), "BRIDGE_BEARER_TOKEN_FILE") {
		t.Fatalf("runtimeConfigFromEnvironment() error = %v", err)
	}
}

func TestBackendSecretFileErrorsPropagate(t *testing.T) {
	tests := []struct {
		name        string
		backendType string
		configure   func(*testing.T)
		want        string
	}{
		{
			name:        "SigNoz API key",
			backendType: "signoz",
			configure: func(t *testing.T) {
				unsetEnvironment(t, "BRIDGE_SIGNOZ_API_KEY")
				t.Setenv("BRIDGE_SIGNOZ_API_KEY_FILE", "/does/not/exist")
			},
			want: "load SigNoz API key",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			test.configure(t)
			_, err := backendFromEnvironment(
				test.backendType,
				backendHTTPClient(time.Second),
				4<<20,
				5_000,
				100_000,
			)
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("backendFromEnvironment() error = %v, want match %q", err, test.want)
			}
		})
	}
}

func TestBackendHTTPClientRejectsCredentialBearingRedirects(t *testing.T) {
	var targetRequests atomic.Int64
	target := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		targetRequests.Add(1)
	}))
	t.Cleanup(target.Close)

	source := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		http.Redirect(response, request, target.URL, http.StatusTemporaryRedirect)
	}))
	t.Cleanup(source.Close)

	request, err := http.NewRequest(http.MethodGet, source.URL, nil)
	if err != nil {
		t.Fatalf("create request: %v", err)
	}
	request.Header.Set("SIGNOZ-API-KEY", "signoz-secret")
	request.Header.Set("Authorization", "Bearer bridge-secret")

	response, err := backendHTTPClient(time.Second).Do(request)
	if response != nil {
		_ = response.Body.Close()
	}
	if err == nil || !strings.Contains(err.Error(), "backend redirects are disabled") {
		t.Fatalf("Do() error = %v, want redirect rejection", err)
	}
	if targetRequests.Load() != 0 {
		t.Fatalf("redirect target received %d request(s), want 0", targetRequests.Load())
	}
}

func unsetEnvironment(t *testing.T, name string) {
	t.Helper()
	value, configured := os.LookupEnv(name)
	if err := os.Unsetenv(name); err != nil {
		t.Fatalf("unset %s: %v", name, err)
	}
	t.Cleanup(func() {
		if configured {
			if err := os.Setenv(name, value); err != nil {
				t.Errorf("restore %s: %v", name, err)
			}
			return
		}
		if err := os.Unsetenv(name); err != nil {
			t.Errorf("unset %s during cleanup: %v", name, err)
		}
	})
}

func stringPointer(value string) *string {
	return &value
}
