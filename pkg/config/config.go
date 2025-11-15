package config

import (
	"fmt"
	"os"
	"time"

	"gopkg.in/yaml.v3"
)

type Config struct {
	Server    ServerConfig    `yaml:"server"`
	GitLab    GitLabConfig    `yaml:"gitlab"`
	Flintlock FlintlockConfig `yaml:"flintlock"`
	VM        VMConfig        `yaml:"vm"`
	Scheduler SchedulerConfig `yaml:"scheduler"`
	Metrics   MetricsConfig   `yaml:"metrics"`
	Logging   LoggingConfig   `yaml:"logging"`
}

type ServerConfig struct {
	Host         string        `yaml:"host" env:"SERVER_HOST" default:"0.0.0.0"`
	Port         int           `yaml:"port" env:"SERVER_PORT" default:"8080"`
	ReadTimeout  time.Duration `yaml:"read_timeout" default:"30s"`
	WriteTimeout time.Duration `yaml:"write_timeout" default:"30s"`
	TLSEnabled   bool          `yaml:"tls_enabled" env:"SERVER_TLS_ENABLED" default:"false"`
	TLSCertPath  string        `yaml:"tls_cert_path" env:"SERVER_TLS_CERT"`
	TLSKeyPath   string        `yaml:"tls_key_path" env:"SERVER_TLS_KEY"`
}

type GitLabConfig struct {
	URL           string        `yaml:"url" env:"GITLAB_URL"`
	Token         string        `yaml:"token" env:"GITLAB_TOKEN"`
	WebhookSecret string        `yaml:"webhook_secret" env:"GITLAB_WEBHOOK_SECRET"`
	RunnerTags    []string      `yaml:"runner_tags" default:"firecracker,microvm"`
	RunnerTimeout time.Duration `yaml:"runner_timeout" default:"1h"`
	MaxConcurrent int           `yaml:"max_concurrent" default:"10"`
}

type FlintlockConfig struct {
	Endpoint      string        `yaml:"endpoint" env:"FLINTLOCK_ENDPOINT" default:"localhost:9090"`
	Timeout       time.Duration `yaml:"timeout" default:"30s"`
	RetryAttempts int           `yaml:"retry_attempts" default:"3"`
	RetryDelay    time.Duration `yaml:"retry_delay" default:"1s"`
	TLSEnabled    bool          `yaml:"tls_enabled" env:"FLINTLOCK_TLS_ENABLED" default:"false"`
	TLSCACert     string        `yaml:"tls_ca_cert" env:"FLINTLOCK_TLS_CA_CERT"`
	TLSClientCert string        `yaml:"tls_client_cert" env:"FLINTLOCK_TLS_CLIENT_CERT"`
	TLSClientKey  string        `yaml:"tls_client_key" env:"FLINTLOCK_TLS_CLIENT_KEY"`
}

type VMConfig struct {
	DefaultVCPU      int64             `yaml:"default_vcpu" default:"2"`
	DefaultMemoryMB  int64             `yaml:"default_memory_mb" default:"4096"`
	KernelImage      string            `yaml:"kernel_image" default:"ghcr.io/firerunner/kernel:latest"`
	RootFSImage      string            `yaml:"rootfs_image" default:"ghcr.io/firerunner/gitlab-runner:latest"`
	NetworkInterface string            `yaml:"network_interface" default:"eth0"`
	MetadataService  bool              `yaml:"metadata_service" default:"true"`
	CloudInitEnabled bool              `yaml:"cloud_init_enabled" default:"true"`
	ExtraLabels      map[string]string `yaml:"extra_labels"`
}

type SchedulerConfig struct {
	QueueSize         int           `yaml:"queue_size" default:"1000"`
	WorkerCount       int           `yaml:"worker_count" default:"5"`
	JobTimeout        time.Duration `yaml:"job_timeout" default:"2h"`
	CleanupInterval   time.Duration `yaml:"cleanup_interval" default:"5m"`
	VMStartTimeout    time.Duration `yaml:"vm_start_timeout" default:"60s"`
	VMShutdownTimeout time.Duration `yaml:"vm_shutdown_timeout" default:"30s"`
	EnablePrewarming  bool          `yaml:"enable_prewarming" default:"false"`
	PrewarmPoolSize   int           `yaml:"prewarm_pool_size" default:"0"`
}

type MetricsConfig struct {
	Enabled     bool   `yaml:"enabled" env:"METRICS_ENABLED" default:"true"`
	Port        int    `yaml:"port" env:"METRICS_PORT" default:"9090"`
	Path        string `yaml:"path" default:"/metrics"`
	EnablePprof bool   `yaml:"enable_pprof" default:"false"`
	PprofPort   int    `yaml:"pprof_port" default:"6060"`
}

type LoggingConfig struct {
	Level      string `yaml:"level" env:"LOG_LEVEL" default:"info"`
	Format     string `yaml:"format" env:"LOG_FORMAT" default:"json"`
	Output     string `yaml:"output" default:"stdout"`
	MaxSizeMB  int    `yaml:"max_size_mb" default:"100"`
	MaxBackups int    `yaml:"max_backups" default:"3"`
	MaxAgeDays int    `yaml:"max_age_days" default:"28"`
	Compress   bool   `yaml:"compress" default:"true"`
}

func Load(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("failed to read config file: %w", err)
	}

	var cfg Config
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("failed to parse config file: %w", err)
	}

	if err := cfg.applyEnvOverrides(); err != nil {
		return nil, fmt.Errorf("failed to apply env overrides: %w", err)
	}

	if err := cfg.Validate(); err != nil {
		return nil, fmt.Errorf("invalid configuration: %w", err)
	}

	return &cfg, nil
}

func (c *Config) applyEnvOverrides() error {
	if url := os.Getenv("GITLAB_URL"); url != "" {
		c.GitLab.URL = url
	}
	if token := os.Getenv("GITLAB_TOKEN"); token != "" {
		c.GitLab.Token = token
	}
	if secret := os.Getenv("GITLAB_WEBHOOK_SECRET"); secret != "" {
		c.GitLab.WebhookSecret = secret
	}

	if endpoint := os.Getenv("FLINTLOCK_ENDPOINT"); endpoint != "" {
		c.Flintlock.Endpoint = endpoint
	}

	if host := os.Getenv("SERVER_HOST"); host != "" {
		c.Server.Host = host
	}

	return nil
}

func (c *Config) Validate() error {
	if c.GitLab.URL == "" {
		return fmt.Errorf("gitlab.url is required")
	}
	if c.GitLab.Token == "" {
		return fmt.Errorf("gitlab.token is required")
	}
	if c.Flintlock.Endpoint == "" {
		return fmt.Errorf("flintlock.endpoint is required")
	}
	if c.Server.Port < 1 || c.Server.Port > 65535 {
		return fmt.Errorf("invalid server.port: %d", c.Server.Port)
	}
	if c.VM.DefaultVCPU < 1 {
		return fmt.Errorf("vm.default_vcpu must be >= 1")
	}
	if c.VM.DefaultMemoryMB < 512 {
		return fmt.Errorf("vm.default_memory_mb must be >= 512")
	}
	if c.Scheduler.QueueSize < 1 {
		return fmt.Errorf("scheduler.queue_size must be >= 1")
	}
	if c.Scheduler.WorkerCount < 1 {
		return fmt.Errorf("scheduler.worker_count must be >= 1")
	}

	return nil
}

func Default() *Config {
	return &Config{
		Server: ServerConfig{
			Host:         "0.0.0.0",
			Port:         8080,
			ReadTimeout:  30 * time.Second,
			WriteTimeout: 30 * time.Second,
		},
		GitLab: GitLabConfig{
			RunnerTags:    []string{"firecracker", "microvm"},
			RunnerTimeout: 1 * time.Hour,
			MaxConcurrent: 10,
		},
		Flintlock: FlintlockConfig{
			Endpoint:      "localhost:9090",
			Timeout:       30 * time.Second,
			RetryAttempts: 3,
			RetryDelay:    1 * time.Second,
		},
		VM: VMConfig{
			DefaultVCPU:      2,
			DefaultMemoryMB:  4096,
			KernelImage:      "ghcr.io/firerunner/kernel:latest",
			RootFSImage:      "ghcr.io/firerunner/gitlab-runner:latest",
			NetworkInterface: "eth0",
			MetadataService:  true,
			CloudInitEnabled: true,
		},
		Scheduler: SchedulerConfig{
			QueueSize:         1000,
			WorkerCount:       5,
			JobTimeout:        2 * time.Hour,
			CleanupInterval:   5 * time.Minute,
			VMStartTimeout:    60 * time.Second,
			VMShutdownTimeout: 30 * time.Second,
		},
		Metrics: MetricsConfig{
			Enabled: true,
			Port:    9090,
			Path:    "/metrics",
		},
		Logging: LoggingConfig{
			Level:      "info",
			Format:     "json",
			Output:     "stdout",
			MaxSizeMB:  100,
			MaxBackups: 3,
			MaxAgeDays: 28,
			Compress:   true,
		},
	}
}
