package config

import (
	"errors"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

type Config struct {
	Address         string
	DataDir         string
	WebDir          string
	ShutdownTimeout time.Duration
	SampleWindow    int
	DischargeFlow   DischargeConfig
}

// DischargeConfig holds the configurable envelope of the discharge flow
// interlock.
type DischargeConfig struct {
	MaxFlow        float64
	ContactTimeout time.Duration
	TelemetryTTL   time.Duration
}

func DefaultDischargeConfig() DischargeConfig {
	return DischargeConfig{MaxFlow: 100, ContactTimeout: 30 * time.Second, TelemetryTTL: 10 * time.Second}
}

func Load() (Config, error) {
	discharge := DefaultDischargeConfig()
	cfg := Config{
		Address:         env("BIOTREAT_ADDR", "127.0.0.1:19697"),
		DataDir:         env("BIOTREAT_DATA", filepath.Join("data", "events")),
		WebDir:          env("BIOTREAT_WEB", "web"),
		ShutdownTimeout: 8 * time.Second,
		SampleWindow:    10,
		DischargeFlow:   discharge,
	}
	if raw := strings.TrimSpace(os.Getenv("BIOTREAT_DISCHARGE_MAX_FLOW")); raw != "" {
		value, err := strconv.ParseFloat(raw, 64)
		if err != nil {
			return Config{}, errors.New("BIOTREAT_DISCHARGE_MAX_FLOW must be a number")
		}
		cfg.DischargeFlow.MaxFlow = value
	}
	if raw := strings.TrimSpace(os.Getenv("BIOTREAT_DISCHARGE_CONTACT_TIMEOUT")); raw != "" {
		value, err := time.ParseDuration(raw)
		if err != nil {
			return Config{}, errors.New("BIOTREAT_DISCHARGE_CONTACT_TIMEOUT must be a duration, e.g. 30s")
		}
		cfg.DischargeFlow.ContactTimeout = value
	}
	if raw := strings.TrimSpace(os.Getenv("BIOTREAT_DISCHARGE_TELEMETRY_TTL")); raw != "" {
		value, err := time.ParseDuration(raw)
		if err != nil {
			return Config{}, errors.New("BIOTREAT_DISCHARGE_TELEMETRY_TTL must be a duration, e.g. 10s")
		}
		cfg.DischargeFlow.TelemetryTTL = value
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
	if c.DischargeFlow.MaxFlow <= 0 {
		return errors.New("discharge flow limit must be positive")
	}
	if c.DischargeFlow.ContactTimeout <= 0 || c.DischargeFlow.TelemetryTTL <= 0 {
		return errors.New("discharge contact timeout and telemetry ttl must be positive")
	}
	if c.DischargeFlow.ContactTimeout < c.DischargeFlow.TelemetryTTL {
		return errors.New("discharge contact timeout must be no shorter than telemetry ttl")
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
