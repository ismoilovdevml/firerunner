package config

import (
	"os"
	"testing"
	"time"
)

func TestDefault(t *testing.T) {
	cfg := Default()

	if cfg == nil {
		t.Fatal("Default() returned nil")
	}

	if cfg.Server.Port != 8080 {
		t.Errorf("Expected default port 8080, got %d", cfg.Server.Port)
	}

	if cfg.Scheduler.WorkerCount != 5 {
		t.Errorf("Expected default worker count 5, got %d", cfg.Scheduler.WorkerCount)
	}

	if cfg.VM.DefaultVCPU != 2 {
		t.Errorf("Expected default VCPU 2, got %d", cfg.VM.DefaultVCPU)
	}
}

func TestValidate(t *testing.T) {
	tests := []struct {
		name    string
		config  *Config
		wantErr bool
	}{
		{
			name: "valid config",
			config: &Config{
				Server: ServerConfig{Port: 8080},
				GitLab: GitLabConfig{
					URL:   "https://gitlab.com",
					Token: "test-token",
				},
				Flintlock: FlintlockConfig{
					Endpoint: "localhost:9090",
				},
				VM: VMConfig{
					DefaultVCPU:     2,
					DefaultMemoryMB: 4096,
				},
				Scheduler: SchedulerConfig{
					QueueSize:   100,
					WorkerCount: 5,
				},
			},
			wantErr: false,
		},
		{
			name: "missing GitLab URL",
			config: &Config{
				Server: ServerConfig{Port: 8080},
				GitLab: GitLabConfig{
					Token: "test-token",
				},
				Flintlock: FlintlockConfig{
					Endpoint: "localhost:9090",
				},
				VM: VMConfig{
					DefaultVCPU:     2,
					DefaultMemoryMB: 4096,
				},
				Scheduler: SchedulerConfig{
					QueueSize:   100,
					WorkerCount: 5,
				},
			},
			wantErr: true,
		},
		{
			name: "invalid port",
			config: &Config{
				Server: ServerConfig{Port: 99999},
				GitLab: GitLabConfig{
					URL:   "https://gitlab.com",
					Token: "test-token",
				},
				Flintlock: FlintlockConfig{
					Endpoint: "localhost:9090",
				},
				VM: VMConfig{
					DefaultVCPU:     2,
					DefaultMemoryMB: 4096,
				},
				Scheduler: SchedulerConfig{
					QueueSize:   100,
					WorkerCount: 5,
				},
			},
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.config.Validate()
			if (err != nil) != tt.wantErr {
				t.Errorf("Validate() error = %v, wantErr %v", err, tt.wantErr)
			}
		})
	}
}

func TestApplyEnvOverrides(t *testing.T) {
	// Set test environment variables
	os.Setenv("GITLAB_URL", "https://test.gitlab.com")
	os.Setenv("GITLAB_TOKEN", "test-token-from-env")
	defer func() {
		os.Unsetenv("GITLAB_URL")
		os.Unsetenv("GITLAB_TOKEN")
	}()

	cfg := Default()
	err := cfg.applyEnvOverrides()
	if err != nil {
		t.Fatalf("applyEnvOverrides() failed: %v", err)
	}

	if cfg.GitLab.URL != "https://test.gitlab.com" {
		t.Errorf("Expected GitLab URL from env, got %s", cfg.GitLab.URL)
	}

	if cfg.GitLab.Token != "test-token-from-env" {
		t.Errorf("Expected GitLab token from env, got %s", cfg.GitLab.Token)
	}
}

func TestServerConfig(t *testing.T) {
	cfg := &ServerConfig{
		Host:         "0.0.0.0",
		Port:         8080,
		ReadTimeout:  30 * time.Second,
		WriteTimeout: 30 * time.Second,
	}

	if cfg.Host != "0.0.0.0" {
		t.Errorf("Expected host 0.0.0.0, got %s", cfg.Host)
	}

	if cfg.ReadTimeout != 30*time.Second {
		t.Errorf("Expected read timeout 30s, got %v", cfg.ReadTimeout)
	}
}
