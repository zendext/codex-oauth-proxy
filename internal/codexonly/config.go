package codexonly

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"gopkg.in/yaml.v3"
)

const (
	DefaultAuthDir        = "~/.codex"
	DefaultPort           = 8317
	DefaultCodexBaseURL   = "https://chatgpt.com/backend-api/codex"
	DefaultChatGPTBaseURL = "https://chatgpt.com/backend-api"
	DefaultCodexUA        = "codex_cli_rs/0.139.0"
)

type Config struct {
	Host                 string         `yaml:"host"`
	Port                 int            `yaml:"port"`
	AuthDir              string         `yaml:"auth-dir"`
	APIKeys              []string       `yaml:"api-keys"`
	AdminAPIKey          string         `yaml:"admin-api-key"`
	Database             DatabaseConfig `yaml:"database"`
	ProxyURL             string         `yaml:"proxy-url"`
	RequestRetry         int            `yaml:"request-retry"`
	CodexBaseURL         string         `yaml:"codex-base-url"`
	ChatGPTBaseURL       string         `yaml:"chatgpt-base-url"`
	CodexUserAgent       string         `yaml:"codex-user-agent"`
	CodexBetaFeatures    string         `yaml:"codex-beta-features"`
	CodexRefreshTokenURL string         `yaml:"codex-refresh-token-url"`
}

type DatabaseConfig struct {
	Path string `yaml:"path"`
}

func LoadConfig(path string) (*Config, error) {
	cfg := &Config{}
	if strings.TrimSpace(path) != "" {
		raw, err := os.ReadFile(path)
		if err != nil {
			if !os.IsNotExist(err) {
				return nil, fmt.Errorf("load config: %w", err)
			}
		} else if len(strings.TrimSpace(string(raw))) > 0 {
			if err = yaml.Unmarshal(raw, cfg); err != nil {
				return nil, fmt.Errorf("parse config: %w", err)
			}
		}
	}
	ApplyDefaults(cfg)
	return cfg, nil
}

func ApplyDefaults(cfg *Config) {
	if cfg == nil {
		return
	}
	if cfg.Port == 0 {
		cfg.Port = DefaultPort
	}
	if strings.TrimSpace(cfg.AuthDir) == "" {
		cfg.AuthDir = DefaultAuthDir
	}
	if strings.TrimSpace(cfg.CodexBaseURL) == "" {
		cfg.CodexBaseURL = DefaultCodexBaseURL
	}
	if strings.TrimSpace(cfg.ChatGPTBaseURL) == "" {
		cfg.ChatGPTBaseURL = DefaultChatGPTBaseURL
	}
	if cfg.RequestRetry <= 0 {
		cfg.RequestRetry = 3
	}
}

func ResolveAuthDir(path string) (string, error) {
	path = strings.TrimSpace(path)
	if path == "" {
		path = DefaultAuthDir
	}
	if path == "~" {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", fmt.Errorf("resolve home dir: %w", err)
		}
		return home, nil
	}
	if strings.HasPrefix(path, "~/") {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", fmt.Errorf("resolve home dir: %w", err)
		}
		return filepath.Join(home, strings.TrimPrefix(path, "~/")), nil
	}
	return path, nil
}

func ResolveDatabasePath(path string, authDir string) (string, error) {
	path = strings.TrimSpace(path)
	if path != "" {
		return expandHome(path)
	}
	authDir, err := ResolveAuthDir(authDir)
	if err != nil {
		return "", err
	}
	return filepath.Join(authDir, "codex-oauth-proxy.db"), nil
}

func expandHome(path string) (string, error) {
	path = strings.TrimSpace(path)
	if path == "~" {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", fmt.Errorf("resolve home dir: %w", err)
		}
		return home, nil
	}
	if strings.HasPrefix(path, "~/") {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", fmt.Errorf("resolve home dir: %w", err)
		}
		return filepath.Join(home, strings.TrimPrefix(path, "~/")), nil
	}
	return path, nil
}
