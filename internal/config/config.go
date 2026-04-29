package config

import (
	"log"
	"os"

	"gopkg.in/yaml.v3"
)

type Config struct {
	ServerURL      string `yaml:"server_url"`
	AgentID        string `yaml:"agent_id"`
	APIKey         string `yaml:"api_key"`
	QueueDir       string `yaml:"queue_dir"`
	TimeoutMs      int    `yaml:"timeout_ms"`
	SNMPCommunity  string `yaml:"snmp_community"`
	RetryInterval  int    `yaml:"retry_interval"`
	ManualFallback bool   `yaml:"manual_fallback"`
	BypassSpooler  bool   `yaml:"bypass_spooler"`
	LogLevel       string `yaml:"log_level"`
}

func Load(path string) *Config {
	cfg := &Config{
		AgentID:        "PORTABLE-001",
		QueueDir:       "./queue",
		TimeoutMs:      3000,
		SNMPCommunity:  "public",
		RetryInterval:  60,
		ManualFallback: true,
		BypassSpooler:  false,
		LogLevel:       "info",
	}

	data, err := os.ReadFile(path)
	if err != nil {
		log.Printf("[Config] %s no encontrado, usando valores por defecto", path)
		return cfg
	}

	if err := yaml.Unmarshal(data, cfg); err != nil {
		log.Fatalf("[Config] Error parseando %s: %v", path, err)
	}

	return cfg
}
