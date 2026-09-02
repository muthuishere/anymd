package llm

import (
	"crypto/tls"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

// ConfigFileName is the per-machine config file, read from the user's config
// directory (on macOS and Linux: ~/.config/anymd/anymdconfig.json).
//
// Config lives outside the repo on purpose: it holds machine-specific things —
// a proxy, a base URL, which model this box is allowed to call — and it must
// never be committed.
const ConfigFileName = "anymdconfig.json"

// FileConfig is the on-disk shape. Every string field supports ${VAR}
// interpolation from the environment, which is how a key stays out of the file:
//
//	{
//	  "model":    "openai/gpt-4o-mini",
//	  "base_url": "https://openrouter.ai/api/v1",
//	  "api_key":  "${LLM_API_KEY}",
//	  "proxy":    "http://127.0.0.1:8080",
//	  "headers":  { "HTTP-Referer": "${SITE_URL}" },
//	  "timeout_ms": 60000
//	}
type FileConfig struct {
	Model     string            `json:"model"`
	BaseURL   string            `json:"base_url"`
	APIKey    string            `json:"api_key"`
	Anthropic bool              `json:"anthropic"`
	Prompt    string            `json:"prompt"`
	Retries   int               `json:"retries"`
	TimeoutMs int               `json:"timeout_ms"`
	Proxy     string            `json:"proxy"`
	Headers   map[string]string `json:"headers"`

	// InsecureSkipVerify disables TLS verification. For a self-signed
	// corporate proxy only; it makes the connection interceptable.
	InsecureSkipVerify bool `json:"insecure_skip_verify"`
}

// ConfigPath returns the path Load reads when given "".
//
// It is $XDG_CONFIG_HOME/anymd/ when that is set, otherwise ~/.config/anymd/ —
// on every platform including macOS. This deliberately does NOT use
// os.UserConfigDir(), which resolves to ~/Library/Application Support on macOS:
// a CLI's config belongs where a developer expects to find and edit it, next to
// every other ~/.config tool.
func ConfigPath() (string, error) {
	if dir := os.Getenv("XDG_CONFIG_HOME"); dir != "" {
		return filepath.Join(dir, "anymd", ConfigFileName), nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".config", "anymd", ConfigFileName), nil
}

var envRef = regexp.MustCompile(`\$\{([A-Za-z_][A-Za-z0-9_]*)\}`)

// expandEnv replaces every ${VAR} with the environment's value.
//
// An unset variable is an ERROR, not an empty string. Silently expanding a
// missing key to "" would send an unauthenticated request and surface as a
// confusing 401 from the provider; naming the variable is far more useful.
// The error names the VARIABLE, never its value.
func expandEnv(field, s string) (string, error) {
	var bad []string
	out := envRef.ReplaceAllStringFunc(s, func(m string) string {
		name := envRef.FindStringSubmatch(m)[1]
		v, ok := os.LookupEnv(name)
		if !ok || v == "" {
			bad = append(bad, name)
			return ""
		}
		return v
	})
	if len(bad) > 0 {
		return "", fmt.Errorf("llm: %s references unset environment variable(s): %s",
			field, strings.Join(bad, ", "))
	}
	return out, nil
}

// Load reads the config file and returns a Config ready for New.
//
// path may be empty, in which case ConfigPath() is used. A missing file is NOT
// an error: it yields the zero Config, so anymd works with environment
// variables alone and the file is purely optional.
func Load(path string) (Config, error) {
	if path == "" {
		p, err := ConfigPath()
		if err != nil {
			return Config{}, err
		}
		path = p
	}
	raw, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return Config{}, nil
	}
	if err != nil {
		return Config{}, fmt.Errorf("llm: reading %s: %w", path, err)
	}

	var fc FileConfig
	if err := json.Unmarshal(raw, &fc); err != nil {
		return Config{}, fmt.Errorf("llm: parsing %s: %w", path, err)
	}
	return fc.toConfig(path)
}

func (fc FileConfig) toConfig(path string) (Config, error) {
	// Expand every string field. Note the error messages name the FIELD and the
	// VARIABLE — never a value, so a key cannot leak into a log.
	for _, f := range []struct {
		name string
		p    *string
	}{
		{"model", &fc.Model},
		{"base_url", &fc.BaseURL},
		{"api_key", &fc.APIKey},
		{"prompt", &fc.Prompt},
		{"proxy", &fc.Proxy},
	} {
		v, err := expandEnv(f.name, *f.p)
		if err != nil {
			return Config{}, fmt.Errorf("%w (in %s)", err, path)
		}
		*f.p = v
	}
	for k, v := range fc.Headers {
		ev, err := expandEnv("headers."+k, v)
		if err != nil {
			return Config{}, fmt.Errorf("%w (in %s)", err, path)
		}
		fc.Headers[k] = ev
	}

	cfg := Config{
		Model:     fc.Model,
		BaseURL:   fc.BaseURL,
		APIKey:    fc.APIKey,
		Anthropic: fc.Anthropic,
		Prompt:    fc.Prompt,
		Retries:   fc.Retries,
		TimeoutMs: fc.TimeoutMs,
		Headers:   fc.Headers,
	}

	if fc.Proxy != "" || fc.InsecureSkipVerify {
		hc, err := httpClientFor(fc)
		if err != nil {
			return Config{}, fmt.Errorf("%w (in %s)", err, path)
		}
		cfg.HTTPClient = hc
	}
	return cfg, nil
}

// httpClientFor builds the proxied client described by the config.
func httpClientFor(fc FileConfig) (*http.Client, error) {
	tr := &http.Transport{}
	if fc.Proxy != "" {
		u, err := url.Parse(fc.Proxy)
		if err != nil {
			return nil, fmt.Errorf("llm: invalid proxy URL: %w", err)
		}
		tr.Proxy = http.ProxyURL(u)
	} else {
		tr.Proxy = http.ProxyFromEnvironment
	}
	if fc.InsecureSkipVerify {
		tr.TLSClientConfig = &tls.Config{InsecureSkipVerify: true} //nolint:gosec // opt-in, documented
	}
	timeout := time.Duration(fc.TimeoutMs) * time.Millisecond
	if timeout <= 0 {
		timeout = 60 * time.Second
	}
	return &http.Client{Transport: tr, Timeout: timeout}, nil
}
