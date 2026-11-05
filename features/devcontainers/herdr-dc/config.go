package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
)

// ${XDG_CONFIG_HOME:-~/.config}/herdr-dc/config.json. A missing file or key
// means the default.
type Config struct {
	DockerPath string `json:"dockerPath"`
	SSH        struct {
		Agent       bool   `json:"agent"`
		AgentSocket string `json:"agentSocket"`
	} `json:"ssh"`
}

// JSON decodes over these, so a key the file leaves out keeps its default.
func defaultConfig() Config {
	c := Config{DockerPath: "docker"}
	c.SSH.Agent = true
	return c
}

func configPath() string {
	dir := os.Getenv("XDG_CONFIG_HOME")
	if dir == "" {
		home, _ := os.UserHomeDir()
		dir = filepath.Join(home, ".config")
	}
	return filepath.Join(dir, "herdr-dc", "config.json")
}

var config = sync.OnceValue(func() Config {
	c, err := loadConfig(configPath())
	if err != nil {
		fail(err)
	}
	return c
})

func loadConfig(path string) (Config, error) {
	c := defaultConfig()
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return c, nil
	}
	if err != nil {
		return c, err
	}
	if err := json.Unmarshal(data, &c); err != nil {
		return c, fmt.Errorf("%s: %w", path, err)
	}
	if c.DockerPath == "" {
		c.DockerPath = "docker"
	}
	c.DockerPath = expandHome(c.DockerPath)
	c.SSH.AgentSocket = expandHome(c.SSH.AgentSocket)
	return c, nil
}

func expandHome(p string) string {
	if rest, ok := strings.CutPrefix(p, "~/"); ok {
		home, _ := os.UserHomeDir()
		return filepath.Join(home, rest)
	}
	return p
}
