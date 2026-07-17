package main

import (
	"testing"
)

func TestSanitizeLabel(t *testing.T) {
	tests := []struct {
		name   string
		in     string
		maxLen int
		want   string
	}{
		{"plain", "tmuxtab開発", 20, "tmuxtab開発"},
		{"trims whitespace", "  ログ調査  ", 20, "ログ調査"},
		{"strips quotes", `"PR#123レビュー"`, 20, "PR#123レビュー"},
		{"strips single quotes and backticks", "`'ラベル'`", 20, "ラベル"},
		{"first non-empty line only", "\n\n1行目\n2行目", 20, "1行目"},
		{"truncates by runes not bytes", "あいうえおかきくけこ", 5, "あいうえお"},
		{"removes control chars", "ラベル\tです", 20, "ラベルです"},
		{"empty input", "", 20, ""},
		{"whitespace only", "   \n  \n", 20, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := sanitizeLabel(tt.in, tt.maxLen); got != tt.want {
				t.Errorf("sanitizeLabel(%q, %d) = %q, want %q", tt.in, tt.maxLen, got, tt.want)
			}
		})
	}
}

func TestNonEmptyLines(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want []string
	}{
		{"two lines", "ラベル\n説明", []string{"ラベル", "説明"}},
		{"skips blank lines", "ラベル\n\n  \n説明\n", []string{"ラベル", "説明"}},
		{"empty", "", nil},
		{"single line no newline", "ラベル", []string{"ラベル"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := nonEmptyLines(tt.in)
			if len(got) != len(tt.want) {
				t.Fatalf("nonEmptyLines(%q) = %v, want %v", tt.in, got, tt.want)
			}
			for i := range got {
				if got[i] != tt.want[i] {
					t.Errorf("nonEmptyLines(%q)[%d] = %q, want %q", tt.in, i, got[i], tt.want[i])
				}
			}
		})
	}
}

func TestRedactSecrets(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
	}{
		{"anthropic key", "export ANTHROPIC_API_KEY=sk-ant-api03-AbCdEf1234567890xyz", "export ANTHROPIC_API_KEY=[REDACTED]"},
		{"openai style key", "using sk-proj1234567890abcdefgh now", "using [REDACTED] now"},
		{"github token", "git clone https://ghp_abcdefghijklmnopqrstuvwxyz123456@github.com/x/y", "git clone https://[REDACTED]@github.com/x/y"},
		{"slack token", "token: xoxb-1234567890-abcdef", "token: [REDACTED]"},
		{"aws access key id", "aws_access_key_id = AKIAIOSFODNN7EXAMPLE", "aws_access_key_id = [REDACTED]"},
		{"aws key id bare", "found AKIAIOSFODNN7EXAMPLE in log", "found [REDACTED] in log"},
		{"authorization header", "Authorization: Bearer abc.def.ghi", "Authorization: Bearer [REDACTED]"},
		{"jwt", "payload eyJhbGciOiJIUzI1NiJ9.eyJzdWIiOiIxIn0.signature here", "payload [REDACTED] here"},
		{"env var assignment", "MY_SECRET=hunter2", "MY_SECRET=[REDACTED]"},
		{"yaml style", "password: hunter2", "password: [REDACTED]"},
		{"plain text untouched", "go test ./... passed", "go test ./... passed"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := redactSecrets(tt.in); got != tt.want {
				t.Errorf("redactSecrets(%q) = %q, want %q", tt.in, got, tt.want)
			}
		})
	}
}
