package types

import (
	"github.com/oszuidwest/zwfm-encoder/internal/validation"
	"slices"
	"testing"
)

func TestZabbixConfigValidationIssues(t *testing.T) {
	tests := []struct {
		name      string
		cfg       ZabbixConfig
		wantCodes []string
	}{
		{
			name:      "port=0 accepted (means use default at send time)",
			cfg:       ZabbixConfig{Port: 0},
			wantCodes: nil,
		},
		{
			name:      "port=10051 default accepted",
			cfg:       ZabbixConfig{Port: 10051},
			wantCodes: nil,
		},
		{
			name:      "port=70000 out of range",
			cfg:       ZabbixConfig{Port: 70000},
			wantCodes: []string{ZabbixPortRange},
		},
		{
			name:      "negative port out of range",
			cfg:       ZabbixConfig{Port: -1},
			wantCodes: []string{ZabbixPortRange},
		},
		{
			name:      "half-configured Zabbix accepted at storage level",
			cfg:       ZabbixConfig{Server: "zabbix.example.com", Port: 10051},
			wantCodes: nil,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := zabbixCodes(tt.cfg.ValidationIssues())
			if !slices.Equal(got, tt.wantCodes) {
				t.Fatalf("ValidationIssues() codes = %v, want %v", got, tt.wantCodes)
			}
		})
	}
}
func TestValidateZabbixConfigured(t *testing.T) {
	tests := []struct {
		name                                              string
		server, host, silenceKey, imbalanceKey, uploadKey string
		wantCodes                                         []string
	}{
		{
			name:      "all empty",
			wantCodes: []string{ZabbixServerRequired, ZabbixHostRequired, ZabbixKeyRequired},
		},
		{
			name:      "only server set",
			server:    "zabbix.example.com",
			wantCodes: []string{ZabbixHostRequired, ZabbixKeyRequired},
		},
		{
			name:       "server+host+silence_key OK",
			server:     "zabbix.example.com",
			host:       "encoder-01",
			silenceKey: "silence",
		},
		{
			name:      "server+host+upload_key OK",
			server:    "zabbix.example.com",
			host:      "encoder-01",
			uploadKey: "upload",
		},
		{
			name:         "server+host+imbalance_key OK",
			server:       "zabbix.example.com",
			host:         "encoder-01",
			imbalanceKey: "imbalance",
		},
		{
			name:      "neither key set",
			server:    "zabbix.example.com",
			host:      "encoder-01",
			wantCodes: []string{ZabbixKeyRequired},
		},
		{
			name:      "port-range not validated here",
			server:    "zabbix.example.com",
			host:      "encoder-01",
			uploadKey: "upload",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := zabbixCodes(ValidateZabbixConfigured(
				tt.server,
				tt.host,
				tt.silenceKey,
				tt.imbalanceKey,
				tt.uploadKey,
			))
			if !slices.Equal(got, tt.wantCodes) {
				t.Fatalf("ValidateZabbixConfigured codes = %v, want %v", got, tt.wantCodes)
			}
		})
	}
}
func TestValidateZabbixTarget(t *testing.T) {
	tests := []struct {
		name      string
		server    string
		port      int
		host, key string
		wantCodes []string
	}{
		{
			name:   "all valid",
			server: "zabbix.example.com", port: 10051, host: "encoder-01", key: "silence",
		},
		{
			name:   "port=0 rejected at target level",
			server: "zabbix.example.com", port: 0, host: "encoder-01", key: "silence",
			wantCodes: []string{ZabbixPortRange},
		},
		{
			name:   "port=70000 rejected",
			server: "zabbix.example.com", port: 70000, host: "encoder-01", key: "silence",
			wantCodes: []string{ZabbixPortRange},
		},
		{
			name:   "empty key rejected",
			server: "zabbix.example.com", port: 10051, host: "encoder-01",
			wantCodes: []string{ZabbixKeyRequired},
		},
		{
			name:      "everything missing",
			wantCodes: []string{ZabbixServerRequired, ZabbixHostRequired, ZabbixKeyRequired, ZabbixPortRange},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := zabbixCodes(ValidateZabbixTarget(tt.server, tt.port, tt.host, tt.key))
			if !slices.Equal(got, tt.wantCodes) {
				t.Fatalf("ValidateZabbixTarget codes = %v, want %v", got, tt.wantCodes)
			}
		})
	}
}
func zabbixCodes(issues validation.Issues) []string {
	if len(issues) == 0 {
		return nil
	}
	codes := make([]string, 0, len(issues))
	for _, issue := range issues {
		codes = append(codes, issue.Code)
	}
	return codes
}
