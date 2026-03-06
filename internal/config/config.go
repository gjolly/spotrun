package config

import (
	"fmt"
	"os"

	"gopkg.in/yaml.v3"
)

type Storage struct {
	Type    string `yaml:"type"`
	SizeGiB int64  `yaml:"size_gib"`
}

type Requirements struct {
	VCPUsMin     int32   `yaml:"vcpus_min"`
	MemoryGiBMin float64 `yaml:"memory_gib_min"`
	Arch         string  `yaml:"arch"`
	Storage      Storage `yaml:"storage"`
}

type Workload struct {
	Image     string            `yaml:"image"`
	OutputDir string            `yaml:"output_dir"`
	Env       map[string]string `yaml:"env"`
}

type SpotConfig struct {
	MaxPriceUSDPerHour float64 `yaml:"max_price_usd_per_hour"`
}

type Config struct {
	Regions      []string     `yaml:"regions"`
	Requirements Requirements `yaml:"requirements"`
	Workload     Workload     `yaml:"workload"`
	Spot         SpotConfig   `yaml:"spot"`
}

func Load(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("reading config: %w", err)
	}

	var cfg Config
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("parsing config: %w", err)
	}

	// Set defaults
	if cfg.Requirements.Arch == "" {
		cfg.Requirements.Arch = "any"
	}
	if cfg.Requirements.Storage.Type == "" {
		cfg.Requirements.Storage.Type = "any"
	}
	if cfg.Workload.OutputDir == "" {
		cfg.Workload.OutputDir = "/output"
	}

	// Validate
	if len(cfg.Regions) == 0 {
		return nil, fmt.Errorf("at least one region is required")
	}
	if cfg.Requirements.VCPUsMin <= 0 {
		return nil, fmt.Errorf("requirements.vcpus_min must be > 0")
	}
	if cfg.Requirements.MemoryGiBMin <= 0 {
		return nil, fmt.Errorf("requirements.memory_gib_min must be > 0")
	}
	if cfg.Workload.Image == "" {
		return nil, fmt.Errorf("workload.image is required")
	}

	switch cfg.Requirements.Arch {
	case "amd64", "arm64", "any":
	default:
		return nil, fmt.Errorf("requirements.arch must be amd64, arm64, or any")
	}

	switch cfg.Requirements.Storage.Type {
	case "nvme", "ebs", "any":
	default:
		return nil, fmt.Errorf("requirements.storage.type must be nvme, ebs, or any")
	}

	return &cfg, nil
}
