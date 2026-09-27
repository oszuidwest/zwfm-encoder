// Package dante implements the receive-only Dante-compatible audio input.
//
// This package is derived from Inferno AoIP, Copyright (C) 2023-2025
// Teodor Wozniak, and is licensed under GPL-3.0-or-later.
package dante

import (
	"errors"
	"fmt"
	"net/url"
	"strings"
	"time"
)

const (
	inputScheme        = "dante"
	defaultReorderWait = 4 * time.Millisecond
)

// ErrNotDanteInput indicates that an audio input is not a Dante URI.
var ErrNotDanteInput = errors.New("not a Dante input")

// Config identifies the two transmitter channels used as left and right.
type Config struct {
	Transmitter string
	Channels    [2]string
	Interface   string
	Receiver    string
	ReorderWait time.Duration
}

// IsInput reports whether input selects the native Dante receiver.
func IsInput(input string) bool {
	u, err := url.Parse(input)
	return err == nil && strings.EqualFold(u.Scheme, inputScheme)
}

// ParseInput parses a Dante audio input URI.
//
// The canonical form is:
//
//	dante://transmitter/left-channel/right-channel?interface=eth0
//
// Query parameters tx, left, and right are accepted as an alternative for
// names that are inconvenient to place in the URI path.
func ParseInput(input string) (Config, error) {
	config := Config{ReorderWait: defaultReorderWait}

	u, err := url.Parse(input)
	if err != nil {
		return config, fmt.Errorf("parse Dante input: %w", err)
	}
	if !strings.EqualFold(u.Scheme, inputScheme) {
		return config, ErrNotDanteInput
	}

	query := u.Query()
	config.Transmitter = strings.TrimSpace(query.Get("tx"))
	if config.Transmitter == "" {
		config.Transmitter = strings.TrimSpace(u.Hostname())
	}

	config.Channels[0] = strings.TrimSpace(query.Get("left"))
	config.Channels[1] = strings.TrimSpace(query.Get("right"))
	pathChannels := strings.Split(strings.Trim(u.EscapedPath(), "/"), "/")
	for i := range config.Channels {
		if config.Channels[i] != "" || i >= len(pathChannels) || pathChannels[i] == "" {
			continue
		}
		channel, unescapeErr := url.PathUnescape(pathChannels[i])
		if unescapeErr != nil {
			return config, fmt.Errorf("decode Dante channel %d: %w", i+1, unescapeErr)
		}
		config.Channels[i] = strings.TrimSpace(channel)
	}

	config.Interface = strings.TrimSpace(query.Get("interface"))
	config.Receiver = strings.TrimSpace(query.Get("receiver"))
	if rawWait := strings.TrimSpace(query.Get("reorder")); rawWait != "" {
		config.ReorderWait, err = time.ParseDuration(rawWait)
		if err != nil {
			return config, fmt.Errorf("parse Dante reorder duration: %w", err)
		}
	}

	if config.Transmitter == "" {
		return config, errors.New("dante transmitter is required")
	}
	if config.Channels[0] == "" || config.Channels[1] == "" {
		return config, errors.New("dante left and right channels are required")
	}
	if config.ReorderWait < 0 || config.ReorderWait > time.Second {
		return config, errors.New("dante reorder duration must be between 0 and 1s")
	}

	return config, nil
}
