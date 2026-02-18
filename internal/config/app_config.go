package config

import (
	"fmt"
	"os"
	"time"

	"sigs.k8s.io/yaml"
)

type AppConfig struct {
	Namespace         string `json:"namespace" yaml:"namespace"`
	TemplatePath      string `json:"templatePath" yaml:"templatePath"`
	ValuesPath        string `json:"valuesPath" yaml:"valuesPath"`
	APIAddr           string `json:"apiAddr" yaml:"apiAddr"`
	MetricsAddr       string `json:"metricsAddr" yaml:"metricsAddr"`
	ProbeAddr         string `json:"probeAddr" yaml:"probeAddr"`
	DefaultTTL        string `json:"defaultTTL" yaml:"defaultTTL"`
	ReconcileInterval string `json:"reconcileInterval" yaml:"reconcileInterval"`
}

func Load(path string) (AppConfig, error) {
	var cfg AppConfig
	if path == "" {
		return cfg, nil
	}

	data, err := os.ReadFile(path)
	if err != nil {
		return cfg, fmt.Errorf("read config file: %w", err)
	}
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return cfg, fmt.Errorf("parse config file: %w", err)
	}
	return cfg, nil
}

func ParseDurationOrFallback(v string, fallback time.Duration) time.Duration {
	if v == "" {
		return fallback
	}
	d, err := time.ParseDuration(v)
	if err != nil {
		return fallback
	}
	return d
}
