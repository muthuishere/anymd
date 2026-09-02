// Package llm gives anymd image captioning and OCR by handing images to a
// vision model, the way markitdown's llm_client= parameter does.
//
// It is a subpackage of anymd, so `go get github.com/muthuishere/anymd` brings
// it along. Nothing here runs unless you construct a client and pass it in
// Options.Describer — the default build still makes no network call.
//
//	// markitdown (Python):
//	//   md = MarkItDown(llm_client=OpenAI(), llm_model="gpt-4o")
//	//   md.convert("document_with_images.pdf")
//
//	// anymd (Go):
//	d := llm.New(llm.Config{Model: "gpt-4o"})
//	res, err := anymd.Default().ConvertFile("document_with_images.pdf",
//	    &anymd.Options{Describer: d})
//
// The work of talking to a model — API keys, retries, backoff, and the
// OpenAI-vs-Anthropic wire difference — is done by toolnexus, so this package
// is only the vision message and the glue.
//
// Supplying a Describer means conversion now makes network calls. With
// Options.Describer nil (the default) anymd never touches the network, which is
// what makes it safe to point at untrusted documents.
package llm

import (
	"context"
	"encoding/base64"
	"fmt"
	"net/http"
	"strings"

	tn "github.com/muthuishere/toolnexus/golang"
)

// DefaultPrompt is the instruction sent with each image. It asks for prose
// rather than a list because the result is spliced into a Markdown document.
const DefaultPrompt = "Describe this image for a document index. " +
	"Transcribe any text in it verbatim. Be specific and factual; " +
	"do not speculate about what is not visible. Reply with prose only."

// Config configures a Client. The zero value is usable: it targets OpenRouter
// with a cheap vision model and reads the API key from the environment.
type Config struct {
	// Model is the vision model. Empty means DefaultModel.
	Model string
	// BaseURL overrides the provider endpoint, so a local Ollama or vLLM works
	// as well as a hosted API. Empty means toolnexus's default.
	BaseURL string
	// APIKey is read from the environment when empty (OPENROUTER_API_KEY,
	// OPENAI_API_KEY or ANTHROPIC_API_KEY, per toolnexus). Prefer leaving this
	// empty and setting the environment variable: a key in a config struct
	// tends to end up in a log or a commit.
	APIKey string
	// Anthropic switches to Anthropic's wire format instead of OpenAI's.
	Anthropic bool
	// Prompt overrides DefaultPrompt.
	Prompt string
	// Retries on 429/5xx/network. Zero uses toolnexus's default.
	Retries int
	// TimeoutMs bounds one description. Zero means 60s.
	TimeoutMs int

	// HTTPClient is used for every model request. Supply one to route through a
	// corporate proxy, pin a CA, or set your own timeouts. Nil uses the default
	// client, which still honors HTTP_PROXY/HTTPS_PROXY from the environment.
	HTTPClient *http.Client

	// Headers are added to every request — a gateway's auth header, an
	// OpenRouter HTTP-Referer, a tenant id.
	Headers map[string]string
}

// DefaultModel is a cheap, widely-available vision model. Captioning runs once
// per image, so a document with 50 figures makes 50 calls — the default is
// chosen to keep that affordable. Override it for higher fidelity.
const DefaultModel = "openai/gpt-4o-mini"

// Client implements anymd.Describer on top of a toolnexus client.
type Client struct {
	tn     *tn.Client
	prompt string
}

// New returns a Describer backed by toolnexus.
func New(cfg Config) *Client {
	model := cfg.Model
	if model == "" {
		model = DefaultModel
	}
	timeout := cfg.TimeoutMs
	if timeout == 0 {
		timeout = 60_000
	}
	style := tn.StyleOpenAI
	if cfg.Anthropic {
		style = tn.StyleAnthropic
	}
	prompt := cfg.Prompt
	if prompt == "" {
		prompt = DefaultPrompt
	}
	return &Client{
		tn: tn.CreateClient(tn.ClientOptions{
			Model:      model,
			BaseURL:    cfg.BaseURL,
			APIKey:     cfg.APIKey,
			Style:      style,
			Retries:    cfg.Retries,
			TimeoutMs:  timeout,
			HTTPClient: cfg.HTTPClient,
			Headers:    cfg.Headers,
			MaxTurns:   1, // captioning is one shot; there are no tools to call
		}),
		prompt: prompt,
	}
}

// FromClient wraps a toolnexus client you built yourself.
//
// Use this when Config is not enough — a bespoke transport, a shared client
// across your app, metrics hooks, a custom BodyTransform. anymd then adds
// nothing to your configuration except the vision message.
//
// prompt may be empty, in which case DefaultPrompt is used.
func FromClient(c *tn.Client, prompt string) *Client {
	if prompt == "" {
		prompt = DefaultPrompt
	}
	return &Client{tn: c, prompt: prompt}
}

// FromConfigFile is the one-liner for the common case: read
// ~/.config/anymd/anymdconfig.json (with ${VAR} interpolation) and return a
// ready Describer. A missing file is not an error — you get a client
// configured purely from the environment.
//
// Pass an empty path to use the default location.
func FromConfigFile(path string) (*Client, error) {
	cfg, err := Load(path)
	if err != nil {
		return nil, err
	}
	return New(cfg), nil
}

// Describe implements anymd.Describer.
//
// The image travels as a data: URI inside a content-array user message, which
// is the shape both OpenAI-compatible and Anthropic-compatible vision endpoints
// expect. toolnexus passes history verbatim, so the message is built here and
// toolnexus handles the transport.
func (c *Client) Describe(ctx context.Context, img []byte, mime, hint string) (string, error) {
	if len(img) == 0 {
		return "", fmt.Errorf("llm: empty image")
	}
	if mime == "" {
		mime = "image/png"
	}

	text := c.prompt
	if hint = strings.TrimSpace(hint); hint != "" {
		// The document's own alt text is real signal; give it to the model
		// rather than discarding what the author already wrote.
		text += "\n\nThe document labels this image: " + hint
	}

	history := []any{
		map[string]any{
			"role": "user",
			"content": []any{
				map[string]any{"type": "text", "text": text},
				map[string]any{"type": "image_url", "image_url": map[string]any{
					"url": "data:" + mime + ";base64," + base64.StdEncoding.EncodeToString(img),
				}},
			},
		},
	}

	tk, err := tn.CreateToolkit(ctx, tn.Options{})
	if err != nil {
		return "", fmt.Errorf("llm: toolkit: %w", err)
	}
	// The prompt argument is empty: the real instruction and the image are both
	// in the history message above.
	res, err := c.tn.RunWithHistory(ctx, "", tk, history)
	if err != nil {
		return "", fmt.Errorf("llm: describe: %w", err)
	}
	out := strings.TrimSpace(res.Text)
	if out == "" {
		return "", fmt.Errorf("llm: model returned no description")
	}
	return out, nil
}
