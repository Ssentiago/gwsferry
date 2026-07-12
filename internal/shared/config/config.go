package config

import (
	"fmt"
	"os"
	"path/filepath"
	"sync"

	"github.com/BurntSushi/toml"
)

type Config struct {
	App AppConfig `toml:"app"`
	S3  S3Config  `toml:"s3"`
}

type AppConfig struct {
	Name string `toml:"name"`
}

type S3Config struct {
	AccessKey string `toml:"access_key"`
	SecretKey string `toml:"secret_key"`
	Endpoint  string `toml:"endpoint"`
	Bucket    string `toml:"bucket"`
	Region    string `toml:"region"`
}

var (
	globalConfig *Config
	configMu     sync.RWMutex
)

func Load() (*Config, error) {
	configMu.Lock()
	defer configMu.Unlock()

	execPath, err := os.Executable()
	if err != nil {
		return nil, fmt.Errorf("не удалось определить путь к бинарю: %w", err)
	}
	configPath := filepath.Join(filepath.Dir(execPath), "gwsferry.toml")

	raw, err := os.ReadFile(configPath)
	if err != nil {
		if os.IsNotExist(err) {
			cfg := defaultConfig()
			globalConfig = cfg
			return cfg, nil
		}
		return nil, fmt.Errorf("чтение %s: %w", configPath, err)
	}

	var cfg Config
	if err := toml.Unmarshal(raw, &cfg); err != nil {
		return nil, fmt.Errorf("парсинг %s: %w", configPath, err)
	}

	applyDefaults(&cfg)
	globalConfig = &cfg
	return &cfg, nil
}

func Get() *Config {
	configMu.RLock()
	defer configMu.RUnlock()
	if globalConfig == nil {
		return defaultConfig()
	}
	return globalConfig
}

func defaultConfig() *Config {
	cfg := &Config{
		App: AppConfig{Name: "gwsferry"},
		S3: S3Config{
			Endpoint: "https://s3.amazonaws.com",
			Region:   "us-east-1",
		},
	}
	return cfg
}

func applyDefaults(cfg *Config) {
	if cfg.App.Name == "" {
		cfg.App.Name = "gwsferry"
	}
	if cfg.S3.Endpoint == "" {
		cfg.S3.Endpoint = "https://s3.amazonaws.com"
	}
	if cfg.S3.Region == "" {
		cfg.S3.Region = "us-east-1"
	}
}

func GenerateDefaultConfig() string {
	return `# gwsferry конфигурация

[app]
name = "gwsferry"

[s3]
# Доступ к S3 хранилищу
access_key = ""
secret_key = ""
endpoint = "https://s3.amazonaws.com"
bucket = ""
region = "us-east-1"
`
}
