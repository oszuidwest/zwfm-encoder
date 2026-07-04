package types

import (
	"github.com/oszuidwest/zwfm-encoder/internal/validation"
)

// Zabbix validation rule identifiers. Stored in validation.Issue.Code as
// stable rule identifiers; adapters in config/ switch on these to map
// rules to context-specific messages.
const (
	// ZabbixServerRequired means the trapper server address is empty.
	ZabbixServerRequired = "server_required"
	// ZabbixHostRequired means the monitored host name is empty.
	ZabbixHostRequired = "host_required"
	// ZabbixKeyRequired means none of silence_key, imbalance_key, or upload_key is set
	// (for ValidateZabbixConfigured) or the per-target key is empty
	// (for ValidateZabbixTarget).
	ZabbixKeyRequired = "key_required"
	// ZabbixPortRange means the port is non-zero but outside 1..65535
	// (for stored config) or not in 1..65535 (for a concrete target).
	ZabbixPortRange = "port_range"
)

// ValidationIssues reports stored-Zabbix-config validation issues.
//
// Stored config only enforces port range: port=0 is accepted (means "use
// default at send time"); non-zero must be 1..65535. Completeness
// (server/host/key) is intentionally not enforced at storage time to preserve
// historical behavior; the test-send handler uses ValidateZabbixConfigured for
// that, and runtime senders use ValidateZabbixTarget.
func (c *ZabbixConfig) ValidationIssues() validation.Issues {
	var issues validation.Issues
	if c.Port != 0 && !validation.ValidPort(c.Port) {
		issues = append(issues, validation.Issue{Field: "port", Code: ZabbixPortRange})
	}
	return issues
}

// ValidateZabbixConfigured reports completeness issues for the Zabbix
// test-send handler: server + host + at least one of silence_key/imbalance_key/upload_key.
// Port is intentionally not validated here because the historical handler
// path delegates port-range failures to the runtime (502), not preflight
// (400); ValidateZabbixTarget handles port-range at send time.
func ValidateZabbixConfigured(server, host, silenceKey, imbalanceKey, uploadKey string) validation.Issues {
	var issues validation.Issues
	if server == "" {
		issues = append(issues, validation.Issue{Field: "server", Code: ZabbixServerRequired})
	}
	if host == "" {
		issues = append(issues, validation.Issue{Field: "host", Code: ZabbixHostRequired})
	}
	if silenceKey == "" && imbalanceKey == "" && uploadKey == "" {
		issues = append(issues, validation.Issue{Field: "silence_key", Code: ZabbixKeyRequired})
	}
	return issues
}

// ValidateZabbixTarget reports validation issues for a concrete Zabbix send
// target. All four fields are required and the port must be in 1..65535
// (no zero allowed). Used by runtime send helpers (e.g. SendZabbixTest).
func ValidateZabbixTarget(server string, port int, host, key string) validation.Issues {
	var issues validation.Issues
	if server == "" {
		issues = append(issues, validation.Issue{Field: "server", Code: ZabbixServerRequired})
	}
	if host == "" {
		issues = append(issues, validation.Issue{Field: "host", Code: ZabbixHostRequired})
	}
	if key == "" {
		issues = append(issues, validation.Issue{Field: "key", Code: ZabbixKeyRequired})
	}
	if !validation.ValidPort(port) {
		issues = append(issues, validation.Issue{Field: "port", Code: ZabbixPortRange})
	}
	return issues
}
