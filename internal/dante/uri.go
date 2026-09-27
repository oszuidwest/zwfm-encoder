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
	u, err := url.Parse(input)
	if err != nil {
		return inputConfig{}, inputError(input, "parse Dante input: %v", err)
	}
	if err := validateInputURL(u); err != nil {
		return inputConfig{}, inputError(input, "%v", err)
	}
	left, right, err := parseChannels(u.EscapedPath())
	if err != nil {
		return inputConfig{}, inputError(input, "%v", err)
	}
	interfaceID, err := parseInterfaceQuery(u.RawQuery)
	if err != nil {
		return inputConfig{}, inputError(input, "%v", err)
	}
	return inputConfig{
		transmitter: u.Host,
		left:        left,
		right:       right,
		interfaceID: interfaceID,
	}, nil
}

func validateInputURL(u *url.URL) error {
	if !strings.EqualFold(u.Scheme, "dante") {
		return errors.New("scheme must be dante")
	}
	if u.Host == "" {
		return errors.New("transmitter must not be empty")
	}
	if u.User != nil {
		return errors.New("user information is not supported")
	}
	if u.Fragment != "" {
		return errors.New("fragments are not supported")
	}
	return nil
}

func parseChannels(escapedPath string) (left, right string, err error) {
	if !strings.HasPrefix(escapedPath, "/") {
		return "", "", errors.New("path must start with a slash")
	}
	parts := strings.Split(strings.TrimPrefix(escapedPath, "/"), "/")
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		return "", "", errors.New("path must contain exactly two non-empty channels")
	}
	left, err = url.PathUnescape(parts[0])
	if err != nil || left == "" {
		return "", "", errors.New("left channel is invalid")
	}
	right, err = url.PathUnescape(parts[1])
	if err != nil || right == "" {
		return "", "", errors.New("right channel is invalid")
	}
	return left, right, nil
}

func parseInterfaceQuery(rawQuery string) (string, error) {
	query, err := url.ParseQuery(rawQuery)
	if err != nil {
		return "", fmt.Errorf("query is invalid: %w", err)
	}
	for key := range query {
		if key != "interface" {
			return "", fmt.Errorf("unsupported query parameter %q", key)
		}
	}
	values, ok := query["interface"]
	if !ok {
		return "", nil
	}
	if len(values) != 1 {
		return "", errors.New("interface may be specified only once")
	}
	if values[0] == "" {
		return "", errors.New("interface name must not be empty")
	}
	return values[0], nil
}

func inputError(input, format string, arguments ...any) error {
	detail := fmt.Sprintf(format, arguments...)
	return fmt.Errorf("invalid Dante input %q: %s; expected %s", input, detail, inputFormat)
}
