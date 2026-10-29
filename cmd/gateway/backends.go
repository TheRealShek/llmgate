package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net/url"
	"strconv"
	"strings"
)

type backendConfig struct {
	name          string
	url           *url.URL
	slots         int
	queueCapacity int
}

// loadBackends converts either the legacy single URL or the named list to one
// construction path. Errors never echo URLs, which may contain credentials.
func loadBackends(getenv func(string) string, slots, capacity int) ([]backendConfig, error) {
	if slots <= 0 || capacity < 0 || capacity > int(^uint(0)>>1)-slots {
		return nil, fmt.Errorf("configure backend capacity: requires positive slots and nonnegative queue capacity with representable total")
	}

	raw := getenv("GATEWAY_BACKENDS")

	legacy := getenv("GATEWAY_UPSTREAM_URL")
	if raw == "" {
		if legacy == "" {
			return nil, nil
		}

		target, err := parseUpstreamURL(legacy)
		if err != nil {
			return nil, err
		}

		return []backendConfig{{name: "primary", url: target, slots: slots, queueCapacity: capacity}}, nil
	}

	if legacy != "" {
		return nil, fmt.Errorf("configure backends: set only one of GATEWAY_BACKENDS and GATEWAY_UPSTREAM_URL")
	}

	if len(raw) > 64<<10 {
		return nil, fmt.Errorf("validate GATEWAY_BACKENDS: exceeds 64 KiB")
	}

	var definitions []struct {
		Name          string `json:"name"`
		URL           string `json:"url"`
		Slots         *int   `json:"slots"`
		QueueCapacity *int   `json:"queue_capacity"`
	}

	decoder := json.NewDecoder(strings.NewReader(raw))
	decoder.DisallowUnknownFields()

	if err := decoder.Decode(&definitions); err != nil {
		return nil, fmt.Errorf("validate GATEWAY_BACKENDS: requires a JSON array of backend definitions")
	}

	if decoder.Decode(new(any)) != io.EOF || len(definitions) == 0 || len(definitions) > 16 {
		return nil, fmt.Errorf("validate GATEWAY_BACKENDS: requires one array of 1 to 16 backends")
	}

	targets := make([]backendConfig, 0, len(definitions))

	names := make(map[string]bool, len(definitions))
	for _, definition := range definitions {
		if !validBackendName(definition.Name) || names[definition.Name] {
			return nil, fmt.Errorf("validate GATEWAY_BACKENDS: names must be unique, 1 to 64 ASCII letters, digits, underscores, or hyphens; none is reserved")
		}

		target, err := parseUpstreamURL(definition.URL)
		if err != nil {
			return nil, fmt.Errorf("validate GATEWAY_BACKENDS: requires valid HTTP(S) backend URLs without credentials, query, or fragment")
		}

		entry := backendConfig{name: definition.Name, url: target, slots: slots, queueCapacity: capacity}
		if definition.Slots != nil {
			entry.slots = *definition.Slots
		}

		if definition.QueueCapacity != nil {
			entry.queueCapacity = *definition.QueueCapacity
		}

		if entry.slots <= 0 || entry.queueCapacity < 0 || entry.queueCapacity > int(^uint(0)>>1)-entry.slots {
			return nil, fmt.Errorf("validate GATEWAY_BACKENDS: requires positive slots and nonnegative queue capacity with representable total")
		}

		names[entry.name] = true
		targets = append(targets, entry)
	}

	return targets, nil
}

func validBackendName(name string) bool {
	if name == "" || len(name) > 64 || name == "none" {
		return false
	}

	for _, char := range name {
		if (char >= 'a' && char <= 'z') || (char >= 'A' && char <= 'Z') ||
			(char >= '0' && char <= '9') || char == '_' || char == '-' {
			continue
		}

		return false
	}

	return true
}

func parseUpstreamURL(raw string) (*url.URL, error) {
	upstream, err := url.Parse(raw)
	if err != nil {
		// Parse errors include the raw URL, which may contain credentials.
		return nil, fmt.Errorf("parse GATEWAY_UPSTREAM_URL: invalid URL")
	}

	if (upstream.Scheme != "http" && upstream.Scheme != "https") || upstream.Hostname() == "" {
		return nil, fmt.Errorf("validate GATEWAY_UPSTREAM_URL: requires HTTP or HTTPS and a host")
	}

	if upstream.User != nil ||
		upstream.RawQuery != "" ||
		upstream.ForceQuery ||
		strings.Contains(raw, "#") {
		return nil, fmt.Errorf(
			"validate GATEWAY_UPSTREAM_URL: credentials, query strings, and fragments are unsupported",
		)
	}

	if port := upstream.Port(); port != "" {
		n, err := strconv.Atoi(port)
		if err != nil || n < 1 || n > 65535 {
			return nil, fmt.Errorf("validate GATEWAY_UPSTREAM_URL: port must be between 1 and 65535")
		}
	}

	return upstream, nil
}
