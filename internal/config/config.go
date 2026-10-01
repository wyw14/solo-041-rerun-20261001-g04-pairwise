package config

import (
	"errors"
	"math"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

type Config struct {
	Address             string
	DataDir             string
	WebDir              string
	ShutdownTimeout     time.Duration
	SampleWindow        int
	FlowLimitM3Hour     float64
	FlowTelemetryMaxAge time.Duration
}

func Load() (Config, error) {
	cfg := Config{
		Address:             env("BIOTREAT_ADDR", "127.0.0.1:19697"),
		DataDir:             env("BIOTREAT_DATA", filepath.Join("data", "events")),
		WebDir:              env("BIOTREAT_WEB", "web"),
		ShutdownTimeout:     8 * time.Second,
		SampleWindow:        10,
		FlowLimitM3Hour:     envFloat("BIOTREAT_FLOW_LIMIT_M3H", 1500),
		FlowTelemetryMaxAge: envDuration("BIOTREAT_FLOW_MAX_AGE", 10*time.Second),
	}
	if err := cfg.Validate(); err != nil {
		return Config{}, err
	}
	return cfg, nil
}

func (c Config) Validate() error {
	if strings.TrimSpace(c.Address) == "" {
		return errors.New("listen address is required")
	}
	if _, _, err := net.SplitHostPort(c.Address); err != nil {
		return err
	}
	if c.DataDir == "" || c.WebDir == "" {
		return errors.New("data and web directories are required")
	}
	if c.SampleWindow < 3 || c.SampleWindow > 120 {
		return errors.New("sample window must be between 3 and 120")
	}
	if c.FlowLimitM3Hour <= 0 || math.IsNaN(c.FlowLimitM3Hour) || math.IsInf(c.FlowLimitM3Hour, 0) {
		return errors.New("flow limit must be a positive finite value")
	}
	if c.FlowTelemetryMaxAge < time.Second || c.FlowTelemetryMaxAge > 10*time.Minute {
		return errors.New("flow telemetry max age must be between 1s and 10m")
	}
	return nil
}

func (c Config) EnsureDirectories() error {
	return os.MkdirAll(c.DataDir, 0o755)
}

func env(name, fallback string) string {
	if value := strings.TrimSpace(os.Getenv(name)); value != "" {
		return value
	}
	return fallback
}

func envFloat(name string, fallback float64) float64 {
	value := strings.TrimSpace(os.Getenv(name))
	if value == "" {
		return fallback
	}
	parsed, err := strconv.ParseFloat(value, 64)
	if err != nil {
		return fallback
	}
	return parsed
}

func envDuration(name string, fallback time.Duration) time.Duration {
	value := strings.TrimSpace(os.Getenv(name))
	if value == "" {
		return fallback
	}
	parsed, err := time.ParseDuration(value)
	if err != nil {
		return fallback
	}
	return parsed
}
