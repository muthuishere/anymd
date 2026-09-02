package llm

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeCfg(t *testing.T, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), ConfigFileName)
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestLoadInterpolatesEnvVars(t *testing.T) {
	t.Setenv("LLM_API_KEY", "sk-not-a-real-key-just-a-test")
	t.Setenv("SITE_URL", "https://example.test")
	p := writeCfg(t, `{
      "model": "openai/gpt-4o",
      "base_url": "https://openrouter.ai/api/v1",
      "api_key": "${LLM_API_KEY}",
      "headers": {"HTTP-Referer": "${SITE_URL}"}
    }`)
	cfg, err := Load(p)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if cfg.APIKey != "sk-not-a-real-key-just-a-test" {
		t.Error("api_key was not interpolated")
	}
	if cfg.Headers["HTTP-Referer"] != "https://example.test" {
		t.Errorf("header not interpolated: %q", cfg.Headers["HTTP-Referer"])
	}
	if cfg.Model != "openai/gpt-4o" {
		t.Errorf("model = %q", cfg.Model)
	}
}

// An unset variable must fail loudly. Expanding it to "" would send an
// unauthenticated request and surface as a baffling 401 from the provider.
func TestLoadFailsLoudlyOnUnsetEnvVar(t *testing.T) {
	os.Unsetenv("DEFINITELY_NOT_SET_ANYMD")
	p := writeCfg(t, `{"api_key": "${DEFINITELY_NOT_SET_ANYMD}"}`)
	_, err := Load(p)
	if err == nil {
		t.Fatal("unset variable silently expanded to empty")
	}
	if !strings.Contains(err.Error(), "DEFINITELY_NOT_SET_ANYMD") {
		t.Errorf("error should name the variable, got: %v", err)
	}
}

// The error must name the variable, never its value.
func TestLoadErrorNeverLeaksASecretValue(t *testing.T) {
	t.Setenv("SOME_KEY", "sk-super-secret-value")
	p := writeCfg(t, `{"api_key": "${SOME_KEY}", "base_url": "${ALSO_NOT_SET_ANYMD}"}`)
	_, err := Load(p)
	if err == nil {
		t.Fatal("expected an error")
	}
	if strings.Contains(err.Error(), "sk-super-secret-value") {
		t.Fatalf("error leaked a secret value: %v", err)
	}
}

// A missing file is not an error: the library must work from env alone.
func TestLoadMissingFileIsNotAnError(t *testing.T) {
	cfg, err := Load(filepath.Join(t.TempDir(), "nope.json"))
	if err != nil {
		t.Fatalf("missing file should not error: %v", err)
	}
	if cfg.Model != "" || cfg.APIKey != "" || cfg.BaseURL != "" {
		t.Errorf("expected zero config, got %+v", cfg)
	}
}

func TestLoadBuildsProxiedClient(t *testing.T) {
	p := writeCfg(t, `{"proxy": "http://127.0.0.1:8080"}`)
	cfg, err := Load(p)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if cfg.HTTPClient == nil {
		t.Fatal("proxy config did not produce an HTTP client")
	}
}

func TestLoadRejectsBadProxyURL(t *testing.T) {
	p := writeCfg(t, `{"proxy": "://not a url"}`)
	if _, err := Load(p); err == nil {
		t.Fatal("accepted an invalid proxy URL")
	}
}

func TestLoadRejectsMalformedJSON(t *testing.T) {
	p := writeCfg(t, `{"model": }`)
	if _, err := Load(p); err == nil {
		t.Fatal("accepted malformed JSON")
	}
}

func TestConfigPathIsUnderAnymd(t *testing.T) {
	p, err := ConfigPath()
	if err != nil {
		t.Skip("no user config dir on this platform")
	}
	if filepath.Base(p) != ConfigFileName || filepath.Base(filepath.Dir(p)) != "anymd" {
		t.Errorf("unexpected config path: %s", p)
	}
}
