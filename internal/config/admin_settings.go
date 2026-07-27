package config

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
)

var adminSettingsFile = "admin_settings.json"

type adminSettings struct {
	Authorization          string `json:"authorization"`
	SchedulingMode         string `json:"scheduling_mode,omitempty"`
	PreferredAccountSource string `json:"preferred_account_source,omitempty"`
	PreferredAccountID     string `json:"preferred_account_id,omitempty"`
}

type SchedulingSettings struct {
	Mode            string `json:"mode"`
	PreferredSource string `json:"preferred_source,omitempty"`
	PreferredID     string `json:"preferred_id,omitempty"`
}

// LoadAuthorization loads the value configured from the management console.
// A persisted console value deliberately takes precedence over the environment
// value, so a key changed in the UI survives a restart on a persistent volume.
func LoadAuthorization() string {
	if value := strings.TrimSpace(loadAdminSettings().Authorization); value != "" {
		return value
	}
	return strings.TrimSpace(os.Getenv("Authorization"))
}

func LoadScheduling() SchedulingSettings {
	settings := loadAdminSettings()
	mode := strings.TrimSpace(settings.SchedulingMode)
	if mode == "" {
		mode = "round_robin"
	}
	return SchedulingSettings{
		Mode:            mode,
		PreferredSource: strings.TrimSpace(settings.PreferredAccountSource),
		PreferredID:     strings.TrimSpace(settings.PreferredAccountID),
	}
}

// SaveAuthorization persists and activates the OpenAI-compatible API key.
func SaveAuthorization(value string) error {
	value = strings.TrimSpace(value)
	if value == "" {
		return os.ErrInvalid
	}
	settings := loadAdminSettings()
	settings.Authorization = value
	if err := saveAdminSettings(settings); err != nil {
		return err
	}
	return os.Setenv("Authorization", value)
}

func SaveScheduling(settings SchedulingSettings) error {
	current := loadAdminSettings()
	current.SchedulingMode = strings.TrimSpace(settings.Mode)
	current.PreferredAccountSource = strings.TrimSpace(settings.PreferredSource)
	current.PreferredAccountID = strings.TrimSpace(settings.PreferredID)
	return saveAdminSettings(current)
}

func loadAdminSettings() adminSettings {
	data, err := os.ReadFile(adminSettingsFile)
	if err != nil {
		return adminSettings{}
	}
	var settings adminSettings
	if json.Unmarshal(data, &settings) != nil {
		return adminSettings{}
	}
	return settings
}

func saveAdminSettings(settings adminSettings) error {
	data, err := json.MarshalIndent(settings, "", "  ")
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
	return nil
}
