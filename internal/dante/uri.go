package dante

import (
	"errors"
	"fmt"
	"net/url"
	"strings"
)

const inputFormat = "dante://<transmitter>/<left channel>/<right channel>[?interface=<name>]"

type inputConfig struct {
	transmitter string
	left        string
	right       string
	interfaceID string
}

// IsInput reports whether input is a URL with the dante scheme.
func IsInput(input string) bool {
	u, err := url.Parse(input)
	return err == nil && strings.EqualFold(u.Scheme, "dante")
}

func parseInput(input string) (inputConfig, error) {
	config, err := parseInputURL(input)
	if err != nil {
		return inputConfig{}, fmt.Errorf("invalid Dante input %q: %w; expected %s", input, err, inputFormat)
	}
	return config, nil
}

func parseInputURL(input string) (inputConfig, error) {
	u, err := url.Parse(input)
	switch {
	case err != nil:
		return inputConfig{}, err
	case !strings.EqualFold(u.Scheme, "dante"):
		return inputConfig{}, errors.New("scheme must be dante")
	case u.Host == "":
		return inputConfig{}, errors.New("transmitter must not be empty")
	case u.User != nil:
		return inputConfig{}, errors.New("user information is not supported")
	case u.Fragment != "":
		return inputConfig{}, errors.New("fragments are not supported")
	}

	// Split the escaped path so channel names may contain %2F.
	channels := strings.Split(strings.TrimPrefix(u.EscapedPath(), "/"), "/")
	if len(channels) != 2 || channels[0] == "" || channels[1] == "" {
		return inputConfig{}, errors.New("path must contain exactly two non-empty channels")
	}
	left, leftErr := url.PathUnescape(channels[0])
	right, rightErr := url.PathUnescape(channels[1])
	if err := errors.Join(leftErr, rightErr); err != nil {
		return inputConfig{}, err
	}

	query, err := url.ParseQuery(u.RawQuery)
	if err != nil {
		return inputConfig{}, fmt.Errorf("query is invalid: %w", err)
	}
	for key := range query {
		if key != "interface" {
			return inputConfig{}, fmt.Errorf("unsupported query parameter %q", key)
		}
	}
	interfaceID := query.Get("interface")
	if values, ok := query["interface"]; ok && (len(values) != 1 || interfaceID == "") {
		return inputConfig{}, errors.New("interface must be given once and must not be empty")
	}

	return inputConfig{transmitter: u.Host, left: left, right: right, interfaceID: interfaceID}, nil
}
