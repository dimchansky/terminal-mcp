package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

func fileRootFromConfig(explicit, configPath string) (string, error) {
	if explicit != "" {
		return explicit, nil
	}
	if configPath == "" {
		return ".", nil
	}
	data, err := os.ReadFile(configPath)
	if errors.Is(err, os.ErrNotExist) {
		return ".", nil
	}
	if err != nil {
		return "", fmt.Errorf("read file root configuration: %w", err)
	}
	var config struct {
		FileRoot string `json:"file_root"`
	}
	if err := json.Unmarshal(data, &config); err != nil {
		return "", fmt.Errorf("invalid service config.json: %w", err)
	}
	if config.FileRoot == "" {
		return ".", nil
	}
	if !filepath.IsAbs(config.FileRoot) {
		return "", fmt.Errorf("configured file_root must be an absolute directory")
	}
	return config.FileRoot, nil
}
