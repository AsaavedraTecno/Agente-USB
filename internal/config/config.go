package config

import (
	"fmt"
	"hash/fnv"
	"log"
	"os"
	"path/filepath"
	"strings"

	"gopkg.in/yaml.v3"

	"usb-agent/internal/identity"
)

type Config struct {
	ServerURL       string `yaml:"server_url"`
	AgentID         string `yaml:"agent_id"`
	ClientName      string `yaml:"client_name"`
	APIKey          string `yaml:"api_key"`
	QueueDir        string `yaml:"queue_dir"`
	StateDir        string `yaml:"state_dir"`
	TimeoutMs       int    `yaml:"timeout_ms"`
	SNMPCommunity   string `yaml:"snmp_community"`
	RetryInterval   int    `yaml:"retry_interval"`
	IntervalMinutes int    `yaml:"interval_minutes"`
	ManualFallback  bool   `yaml:"manual_fallback"`
	BypassSpooler   bool   `yaml:"bypass_spooler"`
	LogLevel        string `yaml:"log_level"`
	SkipTLSVerify   bool   `yaml:"skip_tls_verify"`
}

func defaults() *Config {
	return &Config{
		ServerURL:       "https://tdmonitor.cl/api/agent/telemetry",
		AgentID:         "",
		QueueDir:        "./queue",
		StateDir:        "./state",
		TimeoutMs:       3000,
		SNMPCommunity:   "public",
		RetryInterval:   60,
		IntervalMinutes: 30,
		ManualFallback:  true,
		BypassSpooler:   true,
		// false: el default de producción (tdmonitor.cl) tiene certificado
		// válido. Antes daba igual que fuera true porque la respuesta de
		// /agent/config se descartaba entera; ahora el agente actúa sobre
		// ella (pausar el ciclo vía 'active', cambiar 'scan_interval'), así
		// que un MITM dejaría de ser solo teórico -- peor caso: pausar
		// agentes a voluntad simulando active:false. Quien apunte a un
		// servidor propio sin cert válido lo prende a mano en agent.yaml.
		SkipTLSVerify: false,
		LogLevel:      "info",
	}
}

// GetOrCreateAgentID returns this machine's persistent identity.
// La fuente de verdad es un UUID v4 en <ExeDir>/machine_id (ver internal/identity),
// separado de agent.yaml a propósito: reinstalar o pisar la config no debe poder
// borrar la identidad por accidente. Si por algún motivo no se puede leer/escribir
// ese archivo, cae al esquema anterior (derivado del hostname) para no romper
// una instalación existente.
func GetOrCreateAgentID(cfg *Config) string {
	if id, err := identity.GetOrCreateMachineID(ExeDir()); err == nil && id != "" {
		cfg.AgentID = id
		return id
	}

	if cfg.AgentID != "" {
		return cfg.AgentID
	}
	hostname, _ := os.Hostname()
	h := fnv.New32a()
	h.Write([]byte(hostname))
	// Keep hostname to max 10 chars + 6-char hex hash
	short := strings.ToUpper(hostname)
	if len(short) > 10 {
		short = short[:10]
	}
	id := fmt.Sprintf("%s-%06X", short, h.Sum32()&0xFFFFFF)
	cfg.AgentID = id
	_ = cfg.SavePortable()
	return id
}

func Load(path string) *Config {
	cfg := defaults()
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

// ExeDir returns the directory containing the running executable.
func ExeDir() string {
	exe, err := os.Executable()
	if err != nil {
		return "."
	}
	return filepath.Dir(exe)
}

// LoadPortable loads agent.yaml from the same directory as the executable.
func LoadPortable() *Config {
	return Load(filepath.Join(ExeDir(), "agent.yaml"))
}

// Save writes the config to the given path as YAML.
func (c *Config) Save(path string) error {
	data, err := yaml.Marshal(c)
	if err != nil {
		return err
	}
	return os.WriteFile(path, data, 0644)
}

// SavePortable saves the config next to the executable.
func (c *Config) SavePortable() error {
	return c.Save(filepath.Join(ExeDir(), "agent.yaml"))
}

// ResolveDir resolves a relative path against the exe directory.
func (c *Config) ResolveDir(dir string) string {
	if filepath.IsAbs(dir) {
		return dir
	}
	return filepath.Join(ExeDir(), dir)
}
