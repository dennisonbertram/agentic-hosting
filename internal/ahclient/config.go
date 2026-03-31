package ahclient

import (
	"encoding/json"
	"errors"
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

// LoadConfig loads configuration with the following priority:
//  1. Flags (not applicable here — callers should override fields after loading)
//  2. Environment variables: AH_URL, AH_KEY
//  3. Config file at opts.ConfigPath (or ~/.ah/config.json if empty)
//
// Returns an error if no configuration is found.
func LoadConfig(opts LoadOptions) (*Config, error) {
	cfg := &Config{}

	// Priority 2: environment variables
	envURL := os.Getenv("AH_URL")
	envKey := os.Getenv("AH_KEY")
	if envURL != "" || envKey != "" {
		cfg.URL = envURL
		cfg.Key = envKey
		return cfg, nil
	}

	// Priority 3: config file
	cfgPath := opts.ConfigPath
	if cfgPath == "" {
		var err error
		cfgPath, err = DefaultConfigPath()
		if err != nil {
			return nil, errors.New("no configuration found — run 'ahc configure' or set AH_URL and AH_KEY environment variables")
		}
	}

	f, err := os.Open(cfgPath)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, errors.New("no configuration found — run 'ahc configure' or set AH_URL and AH_KEY environment variables")
		}
		return nil, err
	}
	defer f.Close()

	if err := json.NewDecoder(f).Decode(cfg); err != nil {
		return nil, err
	}

	if cfg.URL == "" && cfg.Key == "" {
		return nil, errors.New("no configuration found — run 'ahc configure' or set AH_URL and AH_KEY environment variables")
	}

	return cfg, nil
}

// Save writes the configuration to cfgPath with 0600 permissions.
// Parent directories are created if they do not exist.
func (c *Config) Save(cfgPath string) error {
	if err := os.MkdirAll(filepath.Dir(cfgPath), 0700); err != nil {
		return err
	}

	data, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return err
	}

	return os.WriteFile(cfgPath, data, 0600)
}
