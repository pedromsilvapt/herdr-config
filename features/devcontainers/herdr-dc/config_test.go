package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLoadConfig(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	home, _ := os.UserHomeDir()
	for _, tc := range []struct{ file, want string }{
		{"", "docker"}, // missing
		{`{}`, "docker"},
		{`{"dockerPath": ""}`, "docker"},
		{`{"dockerPath": "podman", "git": {"copyConfig": false}}`, "podman"},
		{`{"dockerPath": "~/bin/docker"}`, filepath.Join(home, "bin/docker")},
	} {
		os.Remove(path)
		if tc.file != "" {
			os.WriteFile(path, []byte(tc.file), 0o644)
		}
		c, err := loadConfig(path)
		if err != nil || c.DockerPath != tc.want {
			t.Errorf("%q: got %q, %v; want %q", tc.file, c.DockerPath, err, tc.want)
		}
	}
	os.WriteFile(path, []byte(`{bad`), 0o644)
	if _, err := loadConfig(path); err == nil {
		t.Error("bad JSON: no error")
	}
}
