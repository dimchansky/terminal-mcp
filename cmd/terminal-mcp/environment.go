package main

import (
	"errors"
	"fmt"
	"os"
	"runtime"
	"slices"
	"strings"
)

// environmentFlags accepts explicit, repeatable --env KEY=VALUE options.
// No inherited environment variable is persisted automatically.
type environmentFlags []string

func (f *environmentFlags) String() string { return fmt.Sprintf("%d explicit override(s)", len(*f)) }

func (f *environmentFlags) Set(s string) error {
	key, value, found := strings.Cut(s, "=")
	if !found {
		return errors.New("env override must use KEY=VALUE")
	}
	if err := validateEnvKey(key); err != nil {
		return err
	}
	if strings.IndexByte(value, 0) >= 0 {
		return fmt.Errorf("env %q: value contains NUL", key)
	}
	*f = append(*f, s)
	return nil
}

func validateEnvKey(key string) error {
	if key == "" || strings.EqualFold(key, "TERMINAL_MCP_TOKEN") {
		return fmt.Errorf("invalid or reserved environment variable name %q", key)
	}
	for i, c := range key {
		if !(c >= 'A' && c <= 'Z' || c >= 'a' && c <= 'z' || c == '_' ||
			i > 0 && c >= '0' && c <= '9') {
			return fmt.Errorf("invalid environment variable name %q: use letters, digits and underscore", key)
		}
	}
	return nil
}

func validateEnvironmentOverrides(overrides map[string]string) error {
	for name, value := range overrides {
		if err := validateEnvKey(name); err != nil {
			return err
		}
		if strings.IndexByte(value, 0) >= 0 {
			return fmt.Errorf("env %q: value contains NUL", name)
		}
	}
	keys := make([]string, 0, len(overrides))
	for k := range overrides {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	seen := make(map[string]string, len(keys))
	for _, key := range keys {
		folded := strings.ToUpper(key)
		if other, found := seen[folded]; found {
			return fmt.Errorf("ambiguous environment override names %q and %q", other, key)
		}
		seen[folded] = key
	}
	return nil
}

// withEnvironmentOptions preserves previous explicit overrides unless the
// operator deliberately clears them. New --env values override saved keys.
func withEnvironmentOptions(saved map[string]string, clear bool, values []string) (map[string]string, error) {
	if !clear {
		if err := validateEnvironmentOverrides(saved); err != nil {
			return nil, err
		}
	}
	var result map[string]string
	if !clear && len(saved) != 0 {
		result = make(map[string]string, len(saved))
		for k, v := range saved {
			result[k] = v
		}
	}
	for _, raw := range values {
		key, value, found := strings.Cut(raw, "=")
		if !found {
			return nil, errors.New("env override must use KEY=VALUE")
		}
		if err := validateEnvKey(key); err != nil {
			return nil, err
		}
		if strings.IndexByte(value, 0) >= 0 {
			return nil, fmt.Errorf("env %q: value contains NUL", key)
		}
		if result == nil {
			result = make(map[string]string)
		}
		for old := range result {
			if strings.EqualFold(old, key) && old != key {
				return nil, fmt.Errorf("ambiguous environment override names %q and %q", old, key)
			}
		}
		result[key] = value
	}
	return result, nil
}

// childEnvironment removes the inherited bearer secret and then applies only
// explicit user overrides. Unlike the old config.path, PATH is inherited from
// each launcher's environment unless the user explicitly supplies --env PATH=...
func childEnvironment(inherited []string, overrides map[string]string) []string {
	env := withoutEnvToken(inherited)
	keys := make([]string, 0, len(overrides))
	for key := range overrides {
		keys = append(keys, key)
	}
	slices.Sort(keys)
	for _, key := range keys {
		filtered := env[:0]
		for _, item := range env {
			existing, _, found := strings.Cut(item, "=")
			if !found || !(existing == key || runtime.GOOS == "windows" && strings.EqualFold(existing, key)) {
				filtered = append(filtered, item)
			}
		}
		env = append(filtered, key+"="+overrides[key])
	}
	return env
}

// validateDirectory checks that the configured long-lived cwd is usable.
// It must not silently fall back to the updater worker's own directory.
func validateDirectory(dir string) error {
	info, err := os.Stat(dir)
	if err != nil {
		return fmt.Errorf("working directory %q: %w", dir, err)
	}
	if !info.IsDir() {
		return fmt.Errorf("working directory %q is not a directory", dir)
	}
	return nil
}
