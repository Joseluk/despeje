package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Duration acepta "90s", "2m"... en el JSON.
type Duration time.Duration

func (d *Duration) UnmarshalJSON(b []byte) error {
	var s string
	if err := json.Unmarshal(b, &s); err != nil {
		return fmt.Errorf("duración inválida %s (usa p.ej. \"2m\")", b)
	}
	v, err := time.ParseDuration(s)
	if err != nil {
		return err
	}
	*d = Duration(v)
	return nil
}

type VPSConfig struct {
	Host       string `json:"host"`
	Port       int    `json:"port"`
	User       string `json:"user"`
	Key        string `json:"key"`
	KnownHosts string `json:"known_hosts"`
}

type Config struct {
	Listen        string    `json:"listen"`
	Mode          Mode      `json:"mode"`
	ISP           string    `json:"isp"`
	Refresh       Duration  `json:"refresh"`
	DirectTimeout Duration  `json:"direct_timeout"`
	Fallback      *bool     `json:"fallback"`
	SystemProxy   *bool     `json:"system_proxy"`
	ViaVPS        []string  `json:"via_vps"`
	Direct        []string  `json:"direct"`
	LogFile       string    `json:"log_file"`
	VPS           VPSConfig `json:"vps"`
}

var validISPs = []string{"any", "movistar", "vodafone", "orange", "digi", "masmovil"}

func LoadConfig(path string) (*Config, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	cfg := &Config{}
	b = bytes.TrimPrefix(b, []byte("\xef\xbb\xbf")) // BOM de Notepad / PowerShell 5
	if err := json.Unmarshal(b, cfg); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	cfg.applyDefaults()
	return cfg, cfg.validate()
}

func (c *Config) applyDefaults() {
	if c.Listen == "" {
		c.Listen = "127.0.0.1:9119"
	}
	if c.Mode == "" {
		c.Mode = ModeAuto
	}
	if c.ISP == "" {
		c.ISP = "any"
	}
	c.ISP = strings.ToLower(c.ISP)
	if c.Refresh == 0 {
		c.Refresh = Duration(2 * time.Minute)
	}
	if c.DirectTimeout == 0 {
		c.DirectTimeout = Duration(4 * time.Second)
	}
	if c.Fallback == nil {
		c.Fallback = ptr(true)
	}
	if c.SystemProxy == nil {
		c.SystemProxy = ptr(true)
	}
	if c.VPS.Port == 0 {
		c.VPS.Port = 22
	}
	if c.VPS.KnownHosts == "" {
		c.VPS.KnownHosts = "~/.ssh/known_hosts"
	}
	c.VPS.Key = expandPath(c.VPS.Key)
	c.VPS.KnownHosts = expandPath(c.VPS.KnownHosts)
	c.LogFile = expandPath(c.LogFile)
}

func (c *Config) validate() error {
	switch c.Mode {
	case ModeAuto, ModeCDN, ModeAll:
	default:
		return fmt.Errorf("mode %q inválido (auto, cdn, all)", c.Mode)
	}
	ok := false
	for _, isp := range validISPs {
		ok = ok || isp == c.ISP
	}
	if !ok {
		return fmt.Errorf("isp %q inválido (%s)", c.ISP, strings.Join(validISPs, ", "))
	}
	if c.VPS.Host == "" || c.VPS.User == "" || c.VPS.Key == "" {
		return fmt.Errorf("faltan vps.host, vps.user o vps.key")
	}
	return nil
}

// expandPath resuelve "~" y variables de entorno, tanto $VAR como %VAR%.
func expandPath(p string) string {
	if p == "" {
		return p
	}
	for {
		i := strings.Index(p, "%")
		j := strings.Index(p[i+1:], "%")
		if i < 0 || j < 0 {
			break
		}
		name := p[i+1 : i+1+j]
		p = p[:i] + os.Getenv(name) + p[i+2+j:]
	}
	p = os.ExpandEnv(p)
	if p == "~" || strings.HasPrefix(p, "~/") || strings.HasPrefix(p, `~\`) {
		if home, err := os.UserHomeDir(); err == nil {
			p = filepath.Join(home, p[1:])
		}
	}
	return filepath.Clean(p)
}

func defaultConfigPath() string {
	exe, err := os.Executable()
	if err != nil {
		return "config.json"
	}
	return filepath.Join(filepath.Dir(exe), "config.json")
}

func ptr[T any](v T) *T { return &v }
