package config

import (
	"fmt"
	"os"
	"path/filepath"
	"sync"

	"github.com/BurntSushi/toml"
)

type Config struct {
	App    AppConfig    `toml:"app"`
	S3     S3Config     `toml:"s3"`
	Yandex YandexConfig `toml:"yandex"`
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

type YandexConfig struct {
	OrgID        string `toml:"org_id"`
	OAuthToken   string `toml:"oauth_token"`
	ClientID     string `toml:"client_id"`
	ClientSecret string `toml:"client_secret"`
	UserWorkers  int `toml:"user_workers"` // параллельных юзеров (1–100, по умолчанию 10)
}

const (
	maxUserWorkers = 100
)

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

// LoadFromFile загружает конфиг из указанного пути.
// Отличается от Load тем, что не ищет файл рядом с бинарём.
func LoadFromFile(path string) (*Config, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("чтение %s: %w", path, err)
	}

	var cfg Config
	if err := toml.Unmarshal(raw, &cfg); err != nil {
		return nil, fmt.Errorf("парсинг %s: %w", path, err)
	}

	applyDefaults(&cfg)
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
		Yandex: YandexConfig{
			UserWorkers: 10,
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
	if cfg.Yandex.UserWorkers <= 0 {
		cfg.Yandex.UserWorkers = 10
	}
	if cfg.Yandex.UserWorkers > maxUserWorkers {
		cfg.Yandex.UserWorkers = maxUserWorkers
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

[yandex]
# OAuth-токен владельца организации Yandex 360
org_id = ""
oauth_token = ""
# Для ExchangeToken (IMAP XOAUTH2)
client_id = ""
client_secret = ""
# Параллелизм: 1–100 юзеров (25 msg workers на юзера, захардкожено)
user_workers = 10
`
}
