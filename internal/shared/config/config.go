package config

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/BurntSushi/toml"
)

type Config struct {
	S3     S3Config     `toml:"s3"`
	Yandex YandexConfig `toml:"yandex"`

	Workspace  string `toml:"workspace"`
	SaKeysDir  string `toml:"sa_keys_dir"`
	Workers    int    `toml:"workers"`
	DestMount  string `toml:"dest_mount"`
	LabelsFile string `toml:"labels_file"`
	StateFile  string `toml:"state_file"`
	MsgWorkers int    `toml:"msg_workers"`
	ImapHost   string `toml:"imap_host"`
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
	UserWorkers  int    `toml:"user_workers"`
}

const (
	AppName        = "gwsferry"
	maxUserWorkers = 100
)

var (
	globalConfig *Config
	configMu     sync.RWMutex
)

// ConfigPath — путь к конфиг-файлу, задаётся через --config в root command.
var ConfigPath string

// CLIOverrides — значения, переданные через CLI-флаги.
// Пустая строка / 0 / false означает "не передано, не переопределять".
type CLIOverrides struct {
	Workspace  string
	SaKeysDir  string
	Workers    int
	DestMount  string
	LabelsFile string
	StateFile  string
	MsgWorkers int
}

// Apply накладывает CLI-флаги поверх загруженного конфига.
func (o CLIOverrides) Apply(cfg *Config) {
	if o.Workspace != "" {
		cfg.Workspace = o.Workspace
	}
	if o.SaKeysDir != "" {
		cfg.SaKeysDir = o.SaKeysDir
	}
	if o.Workers > 0 {
		cfg.Workers = o.Workers
	}
	if o.DestMount != "" {
		cfg.DestMount = o.DestMount
	}
	if o.LabelsFile != "" {
		cfg.LabelsFile = o.LabelsFile
	}
	if o.StateFile != "" {
		cfg.StateFile = o.StateFile
	}
}

// LoadAndResolve загружает конфиг и применяет CLI-overrides.
// Каскад: defaults → config file → CLI flags.
func LoadAndResolve(overrides CLIOverrides) (*Config, error) {
	cfg, err := Load()
	if err != nil {
		return nil, err
	}
	overrides.Apply(cfg)

	if cfg.Workspace == "" {
		return nil, fmt.Errorf("workspace не указан: используйте --workspace или укажите workspace в конфиге")
	}

	return cfg, nil
}

// Load загружает конфиг из файла (путь из --config или рядом с бинарём).
func Load() (*Config, error) {
	configMu.Lock()
	defer configMu.Unlock()

	configPath := resolveConfigPath()

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

// resolveConfigPath определяет путь к конфиг-файлу.
// Приоритет: --config flag > gwsferry.toml рядом с бинарём.
func resolveConfigPath() string {
	if ConfigPath != "" {
		return ConfigPath
	}
	execPath, err := os.Executable()
	if err != nil {
		return "gwsferry.toml"
	}
	return filepath.Join(filepath.Dir(execPath), "gwsferry.toml")
}

func defaultConfig() *Config {
	cfg := &Config{
		S3: S3Config{
			Endpoint: "https://s3.amazonaws.com",
			Region:   "us-east-1",
		},
		Yandex: YandexConfig{
			UserWorkers: 10,
		},
		Workers:    5,
		DestMount:  "/mnt/s3gmail",
		MsgWorkers: 25,
		ImapHost:   "imap.yandex.ru:993",
	}
	return cfg
}

func applyDefaults(cfg *Config) {
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
	if cfg.Workers <= 0 {
		cfg.Workers = 5
	}
	if cfg.DestMount == "" {
		cfg.DestMount = "/mnt/s3gmail"
	}
	if cfg.MsgWorkers <= 0 {
		cfg.MsgWorkers = 25
	}
	if cfg.ImapHost == "" {
		cfg.ImapHost = "imap.yandex.ru:993"
	}
	if cfg.SaKeysDir == "" {
		cfg.SaKeysDir = "workers"
	}

	// Вычисляемые пути
	if cfg.LabelsFile == "" && cfg.Workspace != "" {
		cfg.LabelsFile = fmt.Sprintf("migration_labels_%s.json", cfg.Workspace)
	}
	if cfg.StateFile == "" && cfg.Workspace != "" {
		cfg.StateFile = fmt.Sprintf("migration_gmail_state_%s.json", cfg.Workspace)
	}
}

// UserHomeDir возвращает домашнюю директорию пользователя.
func UserHomeDir() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return "."
	}
	return home
}

// ConfigDir возвращает путь ~/.gwsferry/.
func ConfigDir() string {
	return filepath.Join(UserHomeDir(), ".gwsferry")
}

func GenerateDefaultConfig() string {
	return strings.TrimLeft(`
# gwsferry конфигурация
# Приоритет: CLI-флаги > этот файл > дефолты в коде

# Workspace prefix (обязательный для fetch-labels и copy)
# workspace = "ru"

# Пути к файлам
# sa_keys_dir = "workers"
# labels_file = "migration_labels_ru.json"
# state_file = "migration_gmail_state_ru.json"

# Воркеры
# workers = 5
# dest_mount = "/mnt/s3gmail"

# Yandex IMAP
# imap_host = "imap.yandex.ru:993"
# msg_workers = 25

[s3]
access_key = ""
secret_key = ""
endpoint = "https://s3.amazonaws.com"
bucket = ""
region = "us-east-1"

[yandex]
org_id = ""
oauth_token = ""
client_id = ""
client_secret = ""
user_workers = 10
`, "\n")
}
