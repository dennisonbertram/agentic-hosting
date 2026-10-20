package ahclient

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

// Config holds the URL and API key for the agentic-hosting API.
type Config struct {
	URL string `json:"url"`
	Key string `json:"key"`
}

// LoadOptions controls where LoadConfig looks for configuration.
// ConfigPath, if non-empty, overrides the default ~/.ah/config.json path.
type LoadOptions struct {
	// ConfigPath overrides the default config file location.
	// Leave empty to use the default: ~/.ah/config.json.
	ConfigPath string
}

// DefaultConfigPath returns the default configuration file path (~/.ah/config.json).
func DefaultConfigPath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".ah", "config.json"), nil
}

// ErrNoConfig is returned by LoadConfig when neither the environment nor a
// config file supplies any value.
var ErrNoConfig = errors.New("no configuration found — run 'ahc configure' or set AH_URL and AH_KEY environment variables")

// LoadConfig loads configuration, merging per field. For each of URL and Key
// the priority is:
//  1. Environment variables AH_URL, AH_KEY
//  2. Config file at opts.ConfigPath (or ~/.ah/config.json if empty)
//
// (Flags are applied by callers after loading.) A missing file is not an
// error; an unreadable or malformed file is. Returns ErrNoConfig if nothing
// is configured.
func LoadConfig(opts LoadOptions) (*Config, error) {
	cfg := &Config{}
	// A complete environment configuration stands on its own, so it must not
	// depend on the presence or validity of a saved config file.
	if envURL, envKey := os.Getenv("AH_URL"), os.Getenv("AH_KEY"); envURL != "" && envKey != "" {
		return &Config{URL: envURL, Key: envKey}, nil
	}

	// Load the file first so each environment variable can override only its
	// corresponding field. A partial environment configuration remains useful
	// with the other value supplied by the saved config.
	cfgPath := opts.ConfigPath
	if cfgPath == "" {
		cfgPath, _ = DefaultConfigPath() // empty if no home dir: skip the file
	}
	if cfgPath != "" {
		fileCfg, err := readConfigFile(cfgPath)
		if err != nil {
			return nil, err
		}
		cfg = fileCfg
	}

	if v := os.Getenv("AH_URL"); v != "" {
		cfg.URL = v
	}
	if v := os.Getenv("AH_KEY"); v != "" {
		cfg.Key = v
	}

	if cfg.URL == "" && cfg.Key == "" {
		return nil, ErrNoConfig
	}
	return cfg, nil
}

// LoadConfigFile reads only the config file at cfgPath (the default path if
// empty), ignoring AH_URL and AH_KEY. Use it when the result will be saved
// back, so transient environment credentials are never written to disk.
// Returns ErrNoConfig if the file is missing or holds no values.
func LoadConfigFile(cfgPath string) (*Config, error) {
	if cfgPath == "" {
		var err error
		if cfgPath, err = DefaultConfigPath(); err != nil {
			return nil, err
		}
	}
	cfg, err := readConfigFile(cfgPath)
	if err != nil {
		return nil, err
	}
	if cfg.URL == "" && cfg.Key == "" {
		return nil, ErrNoConfig
	}
	return cfg, nil
}

// readConfigFile parses cfgPath; a missing file yields an empty Config.
func readConfigFile(cfgPath string) (*Config, error) {
	cfg := &Config{}
	data, err := os.ReadFile(cfgPath)
	switch {
	case err == nil:
		if err := json.Unmarshal(data, cfg); err != nil {
			return nil, fmt.Errorf("parse %s: %w", cfgPath, err)
		}
	case !errors.Is(err, os.ErrNotExist):
		return nil, fmt.Errorf("read %s: %w", cfgPath, err)
	}
	return cfg, nil
}

// Save writes the configuration to cfgPath with 0600 permissions, replacing
// any existing file atomically (temp file in the same directory + rename).
// Parent directories are created if they do not exist.
func (c *Config) Save(cfgPath string) error {
	dir := filepath.Dir(cfgPath)
	if err := os.MkdirAll(dir, 0700); err != nil {
		return err
	}

	data, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return err
	}

	tmp, err := os.CreateTemp(dir, ".config-*.tmp") // created 0600
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name()) // no-op after a successful rename
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), cfgPath)
}
