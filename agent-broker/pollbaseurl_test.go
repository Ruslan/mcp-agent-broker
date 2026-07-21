package main

import (
	"net/http"
	"testing"
)

func reqWith(host string, headers map[string]string) *http.Request {
	r := &http.Request{Host: host, Header: http.Header{}}
	for k, v := range headers {
		r.Header.Set(k, v)
	}
	return r
}

func TestPollBaseURL(t *testing.T) {
	tests := []struct {
		name           string
		override       string
		host           string
		headers        map[string]string
		trustForwarded bool
		want           string
	}{
		{
			name:     "override wins and is trimmed",
			override: "https://broker.example/",
			host:     "ignored",
			headers:  map[string]string{"X-Forwarded-Host": "attacker.test"},
			want:     "https://broker.example",
		},
		{
			name: "plain request host, no proxy headers",
			host: "127.0.0.1:9197",
			want: "http://127.0.0.1:9197",
		},
		{
			name:    "X-Forwarded-Proto honored WITHOUT trust flag (the fix)",
			host:    "broker.example",
			headers: map[string]string{"X-Forwarded-Proto": "https"},
			want:    "https://broker.example",
		},
		{
			name:    "X-Forwarded-Proto comma list takes the first, client-facing scheme",
			host:    "broker.example",
			headers: map[string]string{"X-Forwarded-Proto": "https, http"},
			want:    "https://broker.example",
		},
		{
			name:    "junk X-Forwarded-Proto is ignored, not spliced into the URL",
			host:    "broker.example",
			headers: map[string]string{"X-Forwarded-Proto": "javascript"},
			want:    "http://broker.example",
		},
		{
			name:           "X-Forwarded-Host IGNORED without trust flag (token stays on real host)",
			host:           "broker.example",
			headers:        map[string]string{"X-Forwarded-Host": "attacker.test", "X-Forwarded-Proto": "https"},
			trustForwarded: false,
			want:           "https://broker.example",
		},
		{
			name:           "X-Forwarded-Host honored WITH trust flag",
			host:           "internal:9197",
			headers:        map[string]string{"X-Forwarded-Host": "broker.example", "X-Forwarded-Proto": "https"},
			trustForwarded: true,
			want:           "https://broker.example",
		},
		{
			name: "no resolvable host returns empty",
			host: "",
			want: "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := pollBaseURL(reqWith(tt.host, tt.headers), tt.override, tt.trustForwarded)
			if got != tt.want {
				t.Errorf("pollBaseURL() = %q, want %q", got, tt.want)
			}
		})
	}
}
