package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/anthropics/anthropic-sdk-go"
	"github.com/anthropics/anthropic-sdk-go/option"
	"golang.org/x/oauth2"
)

func TestPricePerMTok(t *testing.T) {
	tests := []struct {
		model  string
		in     float64
		out    float64
		wantOK bool
	}{
		{"claude-haiku-4-5", 1.0, 5.0, true},
		{"claude-sonnet-5", 3.0, 15.0, true},
		{"claude-opus-4-8", 5.0, 25.0, true},
		{"gpt-4o", 2.50, 10.00, true},
		{"gpt-4o-mini", 0.15, 0.60, true}, // must not match the gpt-4o entry
		{"gemini-2.5-flash", 0.30, 2.50, true},
		{"gemini-2.5-flash-lite", 0.10, 0.40, true}, // must not match the flash entry
		{"gemini-2.5-pro", 1.25, 10.00, true},
		{"qwen2.5:7b", 0, 0, false},
	}
	for _, tt := range tests {
		t.Run(tt.model, func(t *testing.T) {
			in, out, ok := pricePerMTok(tt.model)
			if ok != tt.wantOK || in != tt.in || out != tt.out {
				t.Errorf("pricePerMTok(%q) = (%v, %v, %v), want (%v, %v, %v)",
					tt.model, in, out, ok, tt.in, tt.out, tt.wantOK)
			}
		})
	}
}

// newTestClient returns an anthropic client pointed at a mock server that
// always responds with the given two-line completion, and a counter of
// received requests.
func newTestClient(t *testing.T, responseText string, usageIn, usageOut int64) (*anthropic.Client, *int) {
	t.Helper()
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		calls++
		resp := map[string]any{
			"id":    "msg_test",
			"type":  "message",
			"role":  "assistant",
			"model": "claude-haiku-4-5",
			"content": []map[string]any{
				{"type": "text", "text": responseText},
			},
			"stop_reason": "end_turn",
			"usage": map[string]any{
				"input_tokens":  usageIn,
				"output_tokens": usageOut,
			},
		}
		rw.Header().Set("Content-Type", "application/json")
		json.NewEncoder(rw).Encode(resp)
	}))
	t.Cleanup(srv.Close)
	client := anthropic.NewClient(
		option.WithBaseURL(srv.URL),
		option.WithAPIKey("test-key"),
		option.WithMaxRetries(0),
	)
	return &client, &calls
}

func TestSummarize(t *testing.T) {
	cfg := config{llm: llmConfig{model: "claude-haiku-4-5"}, lang: "English", maxLabelLen: 20, maxDescLen: 60}
	client, calls := newTestClient(t, "tmuxtab開発\ntmuxtab のテスト実装と動作確認", 4000, 50)
	st := &stats{}
	w := window{id: "@1", name: "zsh", panes: []pane{
		{id: "%1", command: "go", path: "/src/tmuxtab", content: "go test ./..."},
	}}

	label, desc, err := summarize(cfg, &anthropicLLM{client: client}, st, w.id, windowContent(w))
	if err != nil {
		t.Fatalf("summarize returned error: %v", err)
	}
	if label != "tmuxtab開発" {
		t.Errorf("label = %q, want %q", label, "tmuxtab開発")
	}
	if desc != "tmuxtab のテスト実装と動作確認" {
		t.Errorf("desc = %q, want %q", desc, "tmuxtab のテスト実装と動作確認")
	}
	if *calls != 1 {
		t.Errorf("API calls = %d, want 1", *calls)
	}
	if st.calls != 1 || st.inputTokens != 4000 || st.outputTokens != 50 {
		t.Errorf("stats = %+v, want calls=1 input=4000 output=50", *st)
	}
	wantCost := 4000.0/1e6*1.0 + 50.0/1e6*5.0
	if diff := st.costUSD - wantCost; diff > 1e-12 || diff < -1e-12 {
		t.Errorf("costUSD = %v, want %v", st.costUSD, wantCost)
	}
}

func TestSummarizeSingleLineResponse(t *testing.T) {
	cfg := config{llm: llmConfig{model: "claude-haiku-4-5"}, lang: "English", maxLabelLen: 20, maxDescLen: 60}
	client, _ := newTestClient(t, "ラベルのみ", 100, 10)
	st := &stats{}
	w := window{id: "@1", panes: []pane{{id: "%1"}}}

	label, desc, err := summarize(cfg, &anthropicLLM{client: client}, st, w.id, windowContent(w))
	if err != nil {
		t.Fatalf("summarize returned error: %v", err)
	}
	if label != "ラベルのみ" {
		t.Errorf("label = %q, want %q", label, "ラベルのみ")
	}
	if desc != "" {
		t.Errorf("desc = %q, want empty", desc)
	}
}

func TestSummarizeTruncatesLongOutput(t *testing.T) {
	cfg := config{llm: llmConfig{model: "claude-haiku-4-5"}, lang: "English", maxLabelLen: 5, maxDescLen: 10}
	client, _ := newTestClient(t, "あいうえおかきくけこ\n一二三四五六七八九十百千万億兆", 100, 10)
	st := &stats{}
	w := window{id: "@1", panes: []pane{{id: "%1"}}}

	label, desc, err := summarize(cfg, &anthropicLLM{client: client}, st, w.id, windowContent(w))
	if err != nil {
		t.Fatalf("summarize returned error: %v", err)
	}
	if label != "あいうえお" {
		t.Errorf("label = %q, want %q", label, "あいうえお")
	}
	if desc != "一二三四五六七八九十" {
		t.Errorf("desc = %q, want %q", desc, "一二三四五六七八九十")
	}
}

func TestSummarizeUsesConfiguredLanguage(t *testing.T) {
	var gotSystem string
	srv := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		var body struct {
			System []struct {
				Text string `json:"text"`
			} `json:"system"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err == nil && len(body.System) > 0 {
			gotSystem = body.System[0].Text
		}
		rw.Header().Set("Content-Type", "application/json")
		rw.Write([]byte(`{"id":"msg_test","type":"message","role":"assistant","model":"claude-haiku-4-5",
			"content":[{"type":"text","text":"label\ndescription"}],"stop_reason":"end_turn",
			"usage":{"input_tokens":10,"output_tokens":5}}`))
	}))
	defer srv.Close()
	client := anthropic.NewClient(
		option.WithBaseURL(srv.URL),
		option.WithAPIKey("test-key"),
		option.WithMaxRetries(0),
	)
	cfg := config{llm: llmConfig{model: "claude-haiku-4-5"}, lang: "Japanese", maxLabelLen: 20, maxDescLen: 60}
	st := &stats{}
	w := window{id: "@1", panes: []pane{{id: "%1"}}}

	if _, _, err := summarize(cfg, &anthropicLLM{client: &client}, st, w.id, windowContent(w)); err != nil {
		t.Fatalf("summarize returned error: %v", err)
	}
	if !strings.Contains(gotSystem, "Write both lines in Japanese.") {
		t.Errorf("system prompt does not contain the language instruction; got: %q", gotSystem)
	}
}

func TestOpenAILLM(t *testing.T) {
	var gotAuth, gotSystem, gotModel string
	srv := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/chat/completions" {
			t.Errorf("unexpected path %q", r.URL.Path)
		}
		gotAuth = r.Header.Get("Authorization")
		var body struct {
			Model    string `json:"model"`
			Messages []struct {
				Role    string `json:"role"`
				Content string `json:"content"`
			} `json:"messages"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err == nil {
			gotModel = body.Model
			for _, m := range body.Messages {
				if m.Role == "system" {
					gotSystem = m.Content
				}
			}
		}
		rw.Header().Set("Content-Type", "application/json")
		rw.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":"local dev\nrunning tests against a local model"}}],
			"usage":{"prompt_tokens":500,"completion_tokens":15}}`))
	}))
	defer srv.Close()

	llm := &openaiLLM{baseURL: srv.URL, apiKey: "test-key", hc: srv.Client()}
	cfg := config{llm: llmConfig{model: "local-model", priceIn: 1.0, priceOut: 5.0}, lang: "English", maxLabelLen: 20, maxDescLen: 60}
	st := &stats{}

	label, desc, err := summarize(cfg, llm, st, "@1", "## Pane 1\ngo test ./...")
	if err != nil {
		t.Fatalf("summarize returned error: %v", err)
	}
	if label != "local dev" {
		t.Errorf("label = %q, want %q", label, "local dev")
	}
	if desc != "running tests against a local model" {
		t.Errorf("desc = %q, want %q", desc, "running tests against a local model")
	}
	if gotAuth != "Bearer test-key" {
		t.Errorf("Authorization = %q, want Bearer test-key", gotAuth)
	}
	if gotModel != "local-model" {
		t.Errorf("model = %q, want local-model", gotModel)
	}
	if !strings.Contains(gotSystem, "Write both lines in English.") {
		t.Errorf("system prompt missing language instruction: %q", gotSystem)
	}
	if st.inputTokens != 500 || st.outputTokens != 15 {
		t.Errorf("stats tokens = %d/%d, want 500/15", st.inputTokens, st.outputTokens)
	}
	// -price-in/-price-out override applies for models without built-in prices
	wantCost := 500.0/1e6*1.0 + 15.0/1e6*5.0
	if diff := st.costUSD - wantCost; diff > 1e-12 || diff < -1e-12 {
		t.Errorf("costUSD = %v, want %v", st.costUSD, wantCost)
	}
}

func TestGeminiLLM(t *testing.T) {
	var gotPath, gotKey, gotSystem string
	srv := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotKey = r.Header.Get("x-goog-api-key")
		var body struct {
			SystemInstruction struct {
				Parts []struct {
					Text string `json:"text"`
				} `json:"parts"`
			} `json:"system_instruction"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err == nil && len(body.SystemInstruction.Parts) > 0 {
			gotSystem = body.SystemInstruction.Parts[0].Text
		}
		rw.Header().Set("Content-Type", "application/json")
		rw.Write([]byte(`{"candidates":[{"content":{"parts":[{"text":"gemini dev\ntesting the gemini backend"}]}}],
			"usageMetadata":{"promptTokenCount":400,"candidatesTokenCount":12,"thoughtsTokenCount":88}}`))
	}))
	defer srv.Close()

	llm := &geminiLLM{baseURL: srv.URL, apiKey: "gem-key", hc: srv.Client()}
	cfg := config{llm: llmConfig{model: "gemini-2.5-flash-lite"}, lang: "English", maxLabelLen: 20, maxDescLen: 60}
	st := &stats{}

	label, desc, err := summarize(cfg, llm, st, "@1", "## Pane 1\ngo test ./...")
	if err != nil {
		t.Fatalf("summarize returned error: %v", err)
	}
	if label != "gemini dev" {
		t.Errorf("label = %q, want %q", label, "gemini dev")
	}
	if desc != "testing the gemini backend" {
		t.Errorf("desc = %q, want %q", desc, "testing the gemini backend")
	}
	if gotPath != "/models/gemini-2.5-flash-lite:generateContent" {
		t.Errorf("path = %q, want /models/gemini-2.5-flash-lite:generateContent", gotPath)
	}
	if gotKey != "gem-key" {
		t.Errorf("x-goog-api-key = %q, want gem-key", gotKey)
	}
	if !strings.Contains(gotSystem, "Write both lines in English.") {
		t.Errorf("system instruction missing language directive: %q", gotSystem)
	}
	// thinking tokens count as output tokens: 12 + 88
	if st.inputTokens != 400 || st.outputTokens != 100 {
		t.Errorf("stats tokens = %d/%d, want 400/100", st.inputTokens, st.outputTokens)
	}
}

func TestVertexLLM(t *testing.T) {
	var gotPath, gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotAuth = r.Header.Get("Authorization")
		rw.Header().Set("Content-Type", "application/json")
		rw.Write([]byte(`{"candidates":[{"content":{"parts":[{"text":"vertex dev\ntesting the vertex backend"}]}}],
			"usageMetadata":{"promptTokenCount":300,"candidatesTokenCount":10}}`))
	}))
	defer srv.Close()

	llm := &vertexLLM{
		modelURLPrefix: srv.URL + "/projects/my-proj/locations/global/publishers/google/models/",
		ts:             oauth2.StaticTokenSource(&oauth2.Token{AccessToken: "gcp-token"}),
		hc:             srv.Client(),
	}
	text, in, out, err := llm.complete(context.Background(), "gemini-2.5-flash-lite", "sys", "user", 300)
	if err != nil {
		t.Fatalf("complete returned error: %v", err)
	}
	if text != "vertex dev\ntesting the vertex backend" {
		t.Errorf("text = %q", text)
	}
	if in != 300 || out != 10 {
		t.Errorf("tokens = %d/%d, want 300/10", in, out)
	}
	want := "/projects/my-proj/locations/global/publishers/google/models/gemini-2.5-flash-lite:generateContent"
	if gotPath != want {
		t.Errorf("path = %q, want %q", gotPath, want)
	}
	if gotAuth != "Bearer gcp-token" {
		t.Errorf("Authorization = %q, want Bearer gcp-token", gotAuth)
	}
}

func TestOpenAILLMError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		rw.WriteHeader(http.StatusUnauthorized)
		rw.Write([]byte(`{"error":{"message":"invalid api key"}}`))
	}))
	defer srv.Close()

	llm := &openaiLLM{baseURL: srv.URL, hc: srv.Client()}
	if _, _, _, err := llm.complete(context.Background(), "m", "s", "u", 100); err == nil {
		t.Fatal("complete should return an error on non-200 response")
	} else if !strings.Contains(err.Error(), "invalid api key") {
		t.Errorf("error should include response body, got: %v", err)
	}
}

func TestSummarizeAPIError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		rw.Header().Set("Content-Type", "application/json")
		rw.WriteHeader(http.StatusInternalServerError)
		rw.Write([]byte(`{"type":"error","error":{"type":"api_error","message":"boom"}}`))
	}))
	defer srv.Close()
	client := anthropic.NewClient(
		option.WithBaseURL(srv.URL),
		option.WithAPIKey("test-key"),
		option.WithMaxRetries(0),
	)
	cfg := config{llm: llmConfig{model: "claude-haiku-4-5"}, lang: "English", maxLabelLen: 20, maxDescLen: 60}
	st := &stats{}
	w := window{id: "@1", panes: []pane{{id: "%1"}}}

	if _, _, err := summarize(cfg, &anthropicLLM{client: &client}, st, w.id, windowContent(w)); err == nil {
		t.Fatal("summarize should return an error on API failure")
	}
	if st.calls != 0 {
		t.Errorf("stats.calls = %d, want 0 (failed calls must not be counted)", st.calls)
	}
}
