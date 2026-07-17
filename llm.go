package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"strings"

	"github.com/anthropics/anthropic-sdk-go"
	"golang.org/x/oauth2"
	"golang.org/x/oauth2/google"
)

// llmConfig holds provider selection and provider-specific settings.
type llmConfig struct {
	provider string
	baseURL  string
	model    string
	priceIn  float64 // USD per 1M input tokens; 0 = use built-in prices
	priceOut float64 // USD per 1M output tokens; 0 = use built-in prices
	vertex   vertexConfig
}

// vertexConfig holds settings specific to -provider vertex.
type vertexConfig struct {
	project  string
	location string
}

// llmClient is the summarization backend. complete sends one system+user
// exchange and returns the response text with input/output token usage.
type llmClient interface {
	complete(ctx context.Context, model, system, user string, maxTokens int) (text string, inTok, outTok int64, err error)
}

// newLLMClient builds the summarization backend for the configured provider,
// applying provider-specific defaults to cfg.
func newLLMClient(cfg *llmConfig) (llmClient, error) {
	switch cfg.provider {
	case "anthropic":
		if cfg.model == "" {
			cfg.model = "claude-haiku-4-5"
		}
		if os.Getenv("ANTHROPIC_API_KEY") == "" {
			log.Println("warning: ANTHROPIC_API_KEY is not set; API calls will fail unless another credential source is configured")
		}
		c := anthropic.NewClient()
		return &anthropicLLM{client: &c}, nil
	case "openai":
		if cfg.model == "" {
			return nil, fmt.Errorf("-model is required with -provider openai (e.g. -model gpt-4o-mini)")
		}
		base := cfg.baseURL
		if base == "" {
			base = "https://api.openai.com/v1"
		}
		key := os.Getenv("OPENAI_API_KEY")
		if key == "" && cfg.baseURL == "" {
			log.Println("warning: OPENAI_API_KEY is not set; API calls will fail")
		}
		return &openaiLLM{baseURL: strings.TrimRight(base, "/"), apiKey: key, hc: &http.Client{}}, nil
	case "gemini":
		if cfg.model == "" {
			return nil, fmt.Errorf("-model is required with -provider gemini (e.g. -model gemini-2.5-flash-lite)")
		}
		base := cfg.baseURL
		if base == "" {
			base = "https://generativelanguage.googleapis.com/v1beta"
		}
		key := os.Getenv("GEMINI_API_KEY")
		if key == "" {
			key = os.Getenv("GOOGLE_API_KEY")
		}
		if key == "" {
			log.Println("warning: GEMINI_API_KEY is not set; API calls will fail")
		}
		return &geminiLLM{baseURL: strings.TrimRight(base, "/"), apiKey: key, hc: &http.Client{}}, nil
	case "vertex":
		if cfg.model == "" {
			return nil, fmt.Errorf("-model is required with -provider vertex (e.g. -model gemini-2.5-flash-lite)")
		}
		project := cfg.vertex.project
		if project == "" {
			project = os.Getenv("GOOGLE_CLOUD_PROJECT")
		}
		if project == "" {
			return nil, fmt.Errorf("-vertex-project (or GOOGLE_CLOUD_PROJECT) is required with -provider vertex")
		}
		location := cfg.vertex.location
		base := cfg.baseURL
		if base == "" {
			host := "aiplatform.googleapis.com"
			if location != "global" {
				host = location + "-aiplatform.googleapis.com"
			}
			base = "https://" + host + "/v1"
		}
		ts, err := google.DefaultTokenSource(context.Background(), "https://www.googleapis.com/auth/cloud-platform")
		if err != nil {
			return nil, fmt.Errorf("vertex: no GCP credentials found (run `gcloud auth application-default login` or set GOOGLE_APPLICATION_CREDENTIALS): %w", err)
		}
		prefix := fmt.Sprintf("%s/projects/%s/locations/%s/publishers/google/models/",
			strings.TrimRight(base, "/"), project, location)
		return &vertexLLM{modelURLPrefix: prefix, ts: ts, hc: &http.Client{}}, nil
	default:
		return nil, fmt.Errorf("unknown provider %q (anthropic | openai | gemini | vertex)", cfg.provider)
	}
}

// pricePerMTok returns input/output prices in USD per 1M tokens.
// Returns ok=false for unknown models (cost is then logged as unknown).
func pricePerMTok(model string) (in, out float64, ok bool) {
	switch {
	case strings.Contains(model, "haiku"):
		return 1.0, 5.0, true
	case strings.Contains(model, "sonnet"):
		return 3.0, 15.0, true
	case strings.Contains(model, "opus"):
		return 5.0, 25.0, true
	}
	return 0, 0, false
}

type anthropicLLM struct {
	client *anthropic.Client
}

func (a *anthropicLLM) complete(ctx context.Context, model, system, user string, maxTokens int) (string, int64, int64, error) {
	resp, err := a.client.Messages.New(ctx, anthropic.MessageNewParams{
		Model:     anthropic.Model(model),
		MaxTokens: int64(maxTokens),
		System:    []anthropic.TextBlockParam{{Text: system}},
		Messages: []anthropic.MessageParam{
			anthropic.NewUserMessage(anthropic.NewTextBlock(user)),
		},
	})
	if err != nil {
		return "", 0, 0, fmt.Errorf("anthropic API: %w", err)
	}
	var text strings.Builder
	for _, block := range resp.Content {
		if t, ok := block.AsAny().(anthropic.TextBlock); ok {
			text.WriteString(t.Text)
		}
	}
	return text.String(), resp.Usage.InputTokens, resp.Usage.OutputTokens, nil
}

// openaiLLM talks to any OpenAI-compatible chat completions API (OpenAI
// itself, Ollama, llama.cpp, LM Studio, vLLM, ...).
type openaiLLM struct {
	baseURL string
	apiKey  string
	hc      *http.Client
}

func (o *openaiLLM) complete(ctx context.Context, model, system, user string, maxTokens int) (string, int64, int64, error) {
	payload, err := json.Marshal(map[string]any{
		"model": model,
		// max_tokens is the most widely supported field across
		// OpenAI-compatible servers
		"max_tokens": maxTokens,
		"messages": []map[string]string{
			{"role": "system", "content": system},
			{"role": "user", "content": user},
		},
	})
	if err != nil {
		return "", 0, 0, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, o.baseURL+"/chat/completions", bytes.NewReader(payload))
	if err != nil {
		return "", 0, 0, err
	}
	req.Header.Set("Content-Type", "application/json")
	if o.apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+o.apiKey)
	}
	resp, err := o.hc.Do(req)
	if err != nil {
		return "", 0, 0, fmt.Errorf("openai API: %w", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return "", 0, 0, fmt.Errorf("openai API: read response: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return "", 0, 0, fmt.Errorf("openai API: %s: %s", resp.Status, strings.TrimSpace(string(body)))
	}
	var parsed struct {
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
		Usage struct {
			PromptTokens     int64 `json:"prompt_tokens"`
			CompletionTokens int64 `json:"completion_tokens"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(body, &parsed); err != nil {
		return "", 0, 0, fmt.Errorf("openai API: parse response: %w", err)
	}
	if len(parsed.Choices) == 0 {
		return "", 0, 0, fmt.Errorf("openai API: response has no choices")
	}
	return parsed.Choices[0].Message.Content, parsed.Usage.PromptTokens, parsed.Usage.CompletionTokens, nil
}

// geminiLLM talks to the Gemini API (Google AI Studio / Generative Language
// API). Vertex AI uses different auth and endpoints; see vertexLLM.
type geminiLLM struct {
	baseURL string
	apiKey  string
	hc      *http.Client
}

func (g *geminiLLM) complete(ctx context.Context, model, system, user string, maxTokens int) (string, int64, int64, error) {
	url := fmt.Sprintf("%s/models/%s:generateContent", g.baseURL, model)
	return callGenerateContent(ctx, g.hc, url, func(req *http.Request) {
		req.Header.Set("x-goog-api-key", g.apiKey)
	}, system, user, maxTokens)
}

// vertexLLM talks to Gemini models on Vertex AI, authenticating with GCP
// Application Default Credentials.
type vertexLLM struct {
	modelURLPrefix string // ".../projects/{p}/locations/{l}/publishers/google/models/"
	ts             oauth2.TokenSource
	hc             *http.Client
}

func (v *vertexLLM) complete(ctx context.Context, model, system, user string, maxTokens int) (string, int64, int64, error) {
	tok, err := v.ts.Token()
	if err != nil {
		return "", 0, 0, fmt.Errorf("vertex: get access token: %w", err)
	}
	url := v.modelURLPrefix + model + ":generateContent"
	return callGenerateContent(ctx, v.hc, url, func(req *http.Request) {
		req.Header.Set("Authorization", "Bearer "+tok.AccessToken)
	}, system, user, maxTokens)
}

// callGenerateContent sends a generateContent request (shared by the Gemini
// API and Vertex AI, which speak the same schema behind different endpoints
// and auth).
func callGenerateContent(ctx context.Context, hc *http.Client, url string, setAuth func(*http.Request), system, user string, maxTokens int) (string, int64, int64, error) {
	// Gemini 2.5 models spend output tokens on internal thinking before the
	// visible answer; a tight cap can eat the whole budget and return no
	// text, so give generous headroom over the requested maxTokens.
	if maxTokens < 1024 {
		maxTokens = 1024
	}
	payload, err := json.Marshal(map[string]any{
		"system_instruction": map[string]any{
			"parts": []map[string]string{{"text": system}},
		},
		"contents": []map[string]any{
			{"role": "user", "parts": []map[string]string{{"text": user}}},
		},
		"generationConfig": map[string]any{
			"maxOutputTokens": maxTokens,
		},
	})
	if err != nil {
		return "", 0, 0, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(payload))
	if err != nil {
		return "", 0, 0, err
	}
	req.Header.Set("Content-Type", "application/json")
	setAuth(req)
	resp, err := hc.Do(req)
	if err != nil {
		return "", 0, 0, fmt.Errorf("gemini API: %w", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return "", 0, 0, fmt.Errorf("gemini API: read response: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return "", 0, 0, fmt.Errorf("gemini API: %s: %s", resp.Status, strings.TrimSpace(string(body)))
	}
	var parsed struct {
		Candidates []struct {
			Content struct {
				Parts []struct {
					Text string `json:"text"`
				} `json:"parts"`
			} `json:"content"`
		} `json:"candidates"`
		UsageMetadata struct {
			PromptTokenCount     int64 `json:"promptTokenCount"`
			CandidatesTokenCount int64 `json:"candidatesTokenCount"`
			ThoughtsTokenCount   int64 `json:"thoughtsTokenCount"`
		} `json:"usageMetadata"`
	}
	if err := json.Unmarshal(body, &parsed); err != nil {
		return "", 0, 0, fmt.Errorf("gemini API: parse response: %w", err)
	}
	if len(parsed.Candidates) == 0 {
		return "", 0, 0, fmt.Errorf("gemini API: response has no candidates")
	}
	var text strings.Builder
	for _, p := range parsed.Candidates[0].Content.Parts {
		text.WriteString(p.Text)
	}
	// thinking tokens are billed as output tokens
	out := parsed.UsageMetadata.CandidatesTokenCount + parsed.UsageMetadata.ThoughtsTokenCount
	return text.String(), parsed.UsageMetadata.PromptTokenCount, out, nil
}
