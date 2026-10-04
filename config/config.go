package config

import (
	"fmt"
	"os"
	"strings"
	"time"
)

type Config struct {
	Port            string
	ProxyAPIKey     string
	BaseURL         string
	Cookies         []string // one upstream session cookie header per account
	AccountsFile    string
	DefaultModel    string
	ModelCacheTTL   time.Duration
	UpstreamTimeout time.Duration
}

// Load reads configuration from the environment. Upstream sessions come from
// ONYX_COOKIE plus one-per-line entries in ONYX_ACCOUNTS_FILE; each entry is
// the raw cookie header of one logged-in browser session.
func Load() (*Config, error) {
	cfg := &Config{
		Port:            env("PORT", "8090"),
		ProxyAPIKey:     strings.TrimSpace(os.Getenv("PROXY_API_KEY")),
		BaseURL:         strings.TrimRight(env("ONYX_BASE_URL", "https://ai.onyxaxis.org"), "/"),
		AccountsFile:    env("ONYX_ACCOUNTS_FILE", "accounts.txt"),
		DefaultModel:    env("DEFAULT_MODEL", "gpt-5.3"),
		ModelCacheTTL:   5 * time.Minute,
		UpstreamTimeout: 5 * time.Minute,
	}
	if cfg.ProxyAPIKey == "" {
		return nil, fmt.Errorf("PROXY_API_KEY is required")
	}
	if v := os.Getenv("MODEL_CACHE_TTL"); v != "" {
		d, err := time.ParseDuration(v)
		if err != nil || d < time.Second {
			return nil, fmt.Errorf("invalid MODEL_CACHE_TTL: %q", v)
		}
		cfg.ModelCacheTTL = d
	}
	if v := os.Getenv("UPSTREAM_TIMEOUT"); v != "" {
		d, err := time.ParseDuration(v)
		if err != nil || d < 10*time.Second {
			return nil, fmt.Errorf("invalid UPSTREAM_TIMEOUT: %q", v)
		}
		cfg.UpstreamTimeout = d
	}
	if c := strings.TrimSpace(os.Getenv("ONYX_COOKIE")); c != "" {
		cfg.Cookies = append(cfg.Cookies, NormalizeCookie(c))
	}
	fileCookies, err := loadAccountsFile(cfg.AccountsFile)
	if err != nil {
		return nil, err
	}
	cfg.Cookies = append(cfg.Cookies, fileCookies...)
	if len(cfg.Cookies) == 0 {
		return nil, fmt.Errorf("no onyxaxis session configured; set ONYX_COOKIE or %s", cfg.AccountsFile)
	}
	return cfg, nil
}

func loadAccountsFile(path string) ([]string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var out []string
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		out = append(out, NormalizeCookie(line))
	}
	return out, nil
}

// NormalizeCookie accepts a full cookie header ("obsidian_session=...; x=y")
// or a bare session token and returns the header form to replay verbatim.
func NormalizeCookie(s string) string {
	if strings.Contains(s, "=") {
		return s
	}
	return "obsidian_session=" + s
}

func env(key, def string) string {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		return v
	}
	return def
}
