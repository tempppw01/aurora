package config

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
)

const adminSettingsFile = "admin_settings.json"

type adminSettings struct {
	Authorization string `json:"authorization"`
}

// LoadAuthorization loads the value configured from the management console.
// A persisted console value deliberately takes precedence over the environment
// value, so a key changed in the UI survives a restart on a persistent volume.
func LoadAuthorization() string {
	data, err := os.ReadFile(adminSettingsFile)
	if err == nil {
		var settings adminSettings
		if json.Unmarshal(data, &settings) == nil && strings.TrimSpace(settings.Authorization) != "" {
			return strings.TrimSpace(settings.Authorization)
		}
	}
	return strings.TrimSpace(os.Getenv("Authorization"))
}

// SaveAuthorization persists and activates the OpenAI-compatible API key.
func SaveAuthorization(value string) error {
	value = strings.TrimSpace(value)
	if value == "" {
		return os.ErrInvalid
	}
	data, err := json.MarshalIndent(adminSettings{Authorization: value}, "", "  ")
	if err != nil {
		return err
	}
	dir := filepath.Dir(adminSettingsFile)
	tmp, err := os.CreateTemp(dir, ".aurora-settings-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	if err := tmp.Chmod(0600); err != nil {
		tmp.Close()
		return err
	}
	if _, err := tmp.Write(append(data, '\n')); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmpName, adminSettingsFile); err != nil {
		return err
	}
	return os.Setenv("Authorization", value)
}
