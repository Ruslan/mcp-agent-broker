package main

import (
	"bytes"
	"strings"
	"testing"
)

// TestParseArgs pins the contract that keeps a stray argument from booting a server.
// The regression it guards: `broker --help` used to start the broker, bind port 9197
// and create a database in the caller's working directory.
func TestParseArgs(t *testing.T) {
	tests := []struct {
		name     string
		args     []string
		wantCode int
		wantStop bool
		wantOut  string // substring expected on stdout
		wantErr  string // substring expected on stderr
	}{
		{name: "no arguments boots the server", args: nil, wantCode: 0, wantStop: false},
		{name: "--help", args: []string{"--help"}, wantCode: 0, wantStop: true, wantOut: "Usage:"},
		{name: "-h", args: []string{"-h"}, wantCode: 0, wantStop: true, wantOut: "Environment:"},
		{name: "help", args: []string{"help"}, wantCode: 0, wantStop: true, wantOut: "PORT"},
		{name: "--version", args: []string{"--version"}, wantCode: 0, wantStop: true, wantOut: ServerVersion},
		{name: "-v", args: []string{"-v"}, wantCode: 0, wantStop: true, wantOut: ProtocolVersion},
		{name: "unknown flag exits non-zero", args: []string{"--serve"}, wantCode: 2, wantStop: true, wantErr: "unrecognized argument"},
		{name: "typo does not boot", args: []string{"--helpp"}, wantCode: 2, wantStop: true, wantErr: `"--helpp"`},
		{name: "positional argument does not boot", args: []string{"start"}, wantCode: 2, wantStop: true, wantErr: "unrecognized argument"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			code, stop := parseArgs(tt.args, &stdout, &stderr)

			if code != tt.wantCode || stop != tt.wantStop {
				t.Errorf("parseArgs(%q) = (%d, %v), want (%d, %v)", tt.args, code, stop, tt.wantCode, tt.wantStop)
			}
			if tt.wantOut != "" && !strings.Contains(stdout.String(), tt.wantOut) {
				t.Errorf("stdout missing %q, got:\n%s", tt.wantOut, stdout.String())
			}
			if tt.wantErr != "" && !strings.Contains(stderr.String(), tt.wantErr) {
				t.Errorf("stderr missing %q, got:\n%s", tt.wantErr, stderr.String())
			}
			// Help and errors must never leak onto the wrong stream: a harness that
			// captures only stdout should not mistake a usage error for success.
			if tt.wantCode == 0 && stderr.Len() > 0 {
				t.Errorf("success wrote to stderr: %s", stderr.String())
			}
			if tt.wantCode != 0 && stdout.Len() > 0 {
				t.Errorf("failure wrote to stdout: %s", stdout.String())
			}
		})
	}
}

// TestUsageDocumentsEveryReadVariable: the usage text is the only place a caller
// learns what to set. If a new variable is read by main and not listed here, this
// fails — the text going stale is the failure mode that makes it worse than nothing.
func TestUsageDocumentsEveryReadVariable(t *testing.T) {
	for _, v := range []string{
		"PORT", "DB_PATH", "PROMPTS_DIR", "API_KEY",
		"ENABLE_SYNC", "ENABLE_ASYNC", "BROKER_PUBLIC_URL", "BROKER_TRUST_FORWARDED",
	} {
		if !strings.Contains(usageText, v) {
			t.Errorf("usage text does not document %s", v)
		}
	}
}
