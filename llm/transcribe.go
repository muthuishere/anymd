package llm

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/rand"
	"mime/multipart"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/muthuishere/anymd"
)

// Transcriber implements anymd.Transcriber by posting the audio to an
// OpenAI-compatible `/audio/transcriptions` endpoint.
//
// It deliberately does NOT go through toolnexus. toolnexus drives chat
// completions — a JSON body of messages — and transcription is a different
// endpoint with a different wire shape: a multipart upload of the raw audio
// file. Forcing it through the chat client would mean base64-ing the audio into
// a message the endpoint does not accept. So this is plain net/http plus
// mime/multipart from the standard library, and adds no dependency.
type Transcriber struct {
	baseURL string
	apiKey  string
	model   string
	headers map[string]string
	client  *http.Client
	retries int
}

// The whole point of this type: it must satisfy the core interface.
var _ anymd.Transcriber = (*Transcriber)(nil)

// DefaultTranscribeModel is the near-universal speech-to-text model name.
// Every OpenAI-compatible server that implements /audio/transcriptions — the
// hosted API, a local whisper.cpp server, faster-whisper, LM Studio — answers
// to it, which makes it the only safe default.
//
// It is a constructor argument rather than a Config field because Config is
// shared with the Describer and carries the *vision* model in Config.Model;
// reusing that field would send "gpt-4o-mini" to a transcription endpoint.
const DefaultTranscribeModel = "whisper-1"

// DefaultTranscribeBaseURL is used when Config.BaseURL is empty. Unlike
// captioning, there is no sensible aggregator default here: /audio/transcriptions
// is an OpenAI-shaped endpoint, so point BaseURL at whatever server implements
// it (including a local one) when you are not using OpenAI.
const DefaultTranscribeBaseURL = "https://api.openai.com/v1"

// MaxAudioBytes is the largest payload this client will upload. 25 MiB is the
// documented limit of the hosted OpenAI endpoint and the de-facto limit of the
// compatible servers; refusing above it turns a slow upload that ends in a 413
// into an immediate, readable error.
const MaxAudioBytes = 25 << 20

// maxTranscribeResponse caps how much of a response body is read. A hostile or
// broken server must not be able to stream us out of memory.
const maxTranscribeResponse = 8 << 20

// transcribeAPIKeyEnv lists the environment variables consulted for the API
// key, in order. This mirrors what toolnexus does for the Describer, so one
// exported variable configures both.
//
// The key is read from the environment and used only as an Authorization
// header. It is never logged and never appears in an error — errors name the
// VARIABLE, never a value, exactly as config.go's ${VAR} expansion does.
var transcribeAPIKeyEnv = []string{"OPENAI_API_KEY", "OPENROUTER_API_KEY", "LLM_API_KEY"}

// NewTranscriber returns an anymd.Transcriber using the shared Config for
// BaseURL, APIKey, Headers, HTTPClient, TimeoutMs and Retries.
//
// model may be empty, in which case DefaultTranscribeModel is used.
func NewTranscriber(cfg Config, model string) *Transcriber {
	if model == "" {
		model = DefaultTranscribeModel
	}
	base := strings.TrimRight(cfg.BaseURL, "/")
	if base == "" {
		base = DefaultTranscribeBaseURL
	}
	key := cfg.APIKey
	if key == "" {
		for _, name := range transcribeAPIKeyEnv {
			if v := os.Getenv(name); v != "" {
				key = v
				break
			}
		}
	}
	client := cfg.HTTPClient
	if client == nil {
		timeout := time.Duration(cfg.TimeoutMs) * time.Millisecond
		if timeout <= 0 {
			timeout = 60 * time.Second
		}
		client = &http.Client{Timeout: timeout}
	}
	retries := cfg.Retries
	if retries <= 0 {
		retries = 2
	}
	headers := make(map[string]string, len(cfg.Headers))
	for k, v := range cfg.Headers {
		headers[k] = v
	}
	return &Transcriber{
		baseURL: base,
		apiKey:  key,
		model:   model,
		headers: headers,
		client:  client,
		retries: retries,
	}
}

// TranscriberFromConfigFile is the one-liner: read
// ~/.config/anymd/anymdconfig.json (with ${VAR} interpolation) and return a
// ready Transcriber. A missing file is not an error.
//
// model may be empty for DefaultTranscribeModel.
func TranscriberFromConfigFile(path, model string) (*Transcriber, error) {
	cfg, err := Load(path)
	if err != nil {
		return nil, err
	}
	return NewTranscriber(cfg, model), nil
}

// TranscribeError is returned when the endpoint answers with a non-2xx status.
// It carries the status so a caller can distinguish "the audio was rejected"
// from "the provider was down", without the caller having to parse a string.
//
// Body is the server's message, truncated. The request's Authorization header
// is never echoed into it.
type TranscribeError struct {
	Status int
	Body   string
}

func (e *TranscribeError) Error() string {
	if e.Body == "" {
		return fmt.Sprintf("llm: transcription failed: HTTP %d", e.Status)
	}
	return fmt.Sprintf("llm: transcription failed: HTTP %d: %s", e.Status, e.Body)
}

// ErrNoAPIKey is returned when no key was configured and the endpoint is
// remote. A local server (localhost/127.0.0.1) needs no key, so it is allowed
// through unauthenticated.
var ErrNoAPIKey = errors.New("llm: no API key")

// Transcribe implements anymd.Transcriber.
func (t *Transcriber) Transcribe(ctx context.Context, audio []byte, mime string) (string, error) {
	if len(audio) == 0 {
		return "", errors.New("llm: empty audio")
	}
	if len(audio) > MaxAudioBytes {
		return "", fmt.Errorf("llm: audio too large: %d bytes, limit is %d", len(audio), MaxAudioBytes)
	}
	if t.apiKey == "" && !isLocalEndpoint(t.baseURL) {
		return "", fmt.Errorf("%w: set %s in the environment (or api_key in %s)",
			ErrNoAPIKey, strings.Join(transcribeAPIKeyEnv, " or "), ConfigFileName)
	}

	body, contentType, err := t.multipartBody(audio, mime)
	if err != nil {
		return "", err
	}
	endpoint := t.baseURL + "/audio/transcriptions"

	var lastErr error
	for attempt := 0; attempt <= t.retries; attempt++ {
		if attempt > 0 {
			if err := sleepCtx(ctx, backoff(attempt, lastErr)); err != nil {
				return "", err
			}
		}
		text, retryable, err := t.attempt(ctx, endpoint, body, contentType)
		if err == nil {
			return text, nil
		}
		lastErr = err
		if !retryable {
			return "", err
		}
		if ctx.Err() != nil {
			return "", ctx.Err()
		}
	}
	return "", lastErr
}

// multipartBody builds the upload once, so a retry re-sends identical bytes
// rather than rebuilding (and re-randomising) the body.
//
// The file field carries a filename with a real extension: several
// OpenAI-compatible servers dispatch the decoder off the extension and reject
// an extensionless part outright, so "audio" alone silently fails against them.
func (t *Transcriber) multipartBody(audio []byte, mime string) ([]byte, string, error) {
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	fw, err := mw.CreateFormFile("file", "audio"+extForMime(mime))
	if err != nil {
		return nil, "", fmt.Errorf("llm: building request: %w", err)
	}
	if _, err := fw.Write(audio); err != nil {
		return nil, "", fmt.Errorf("llm: building request: %w", err)
	}
	if err := mw.WriteField("model", t.model); err != nil {
		return nil, "", fmt.Errorf("llm: building request: %w", err)
	}
	if err := mw.Close(); err != nil {
		return nil, "", fmt.Errorf("llm: building request: %w", err)
	}
	return buf.Bytes(), mw.FormDataContentType(), nil
}

// attempt performs one request. It reports whether the failure is worth
// retrying: 429 and 5xx are transient, every other 4xx is the caller's fault
// and retrying it only burns quota.
func (t *Transcriber) attempt(ctx context.Context, endpoint string, body []byte, contentType string) (string, bool, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return "", false, fmt.Errorf("llm: transcription request: %w", err)
	}
	req.Header.Set("Content-Type", contentType)
	for k, v := range t.headers {
		req.Header.Set(k, v)
	}
	if t.apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+t.apiKey)
	}

	resp, err := t.client.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return "", false, ctx.Err()
		}
		return "", true, fmt.Errorf("llm: transcription request: %w", err)
	}
	// Always drain and close: an undrained body leaks the connection out of
	// the keep-alive pool.
	defer func() {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, maxTranscribeResponse))
		_ = resp.Body.Close()
	}()

	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxTranscribeResponse))
	if err != nil {
		if ctx.Err() != nil {
			return "", false, ctx.Err()
		}
		return "", true, fmt.Errorf("llm: reading transcription response: %w", err)
	}

	if resp.StatusCode/100 != 2 {
		terr := &TranscribeError{Status: resp.StatusCode, Body: truncate(strings.TrimSpace(string(raw)), 512)}
		retryable := resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode/100 == 5
		if retryable {
			return "", true, &retryAfterError{err: terr, after: parseRetryAfter(resp.Header.Get("Retry-After"))}
		}
		return "", false, terr
	}

	text, err := decodeTranscription(raw)
	if err != nil {
		return "", false, err
	}
	if text == "" {
		// The transcript is the document's entire content; an empty one is a
		// failure, not a successful conversion of silence.
		return "", false, errors.New("llm: transcription returned no text")
	}
	return text, false, nil
}

// decodeTranscription accepts both shapes servers use: the documented JSON
// object with a "text" field, and (for response_format=text) a bare body.
func decodeTranscription(raw []byte) (string, error) {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) > 0 && trimmed[0] == '{' {
		var out struct {
			Text string `json:"text"`
		}
		if err := json.Unmarshal(trimmed, &out); err != nil {
			return "", fmt.Errorf("llm: decoding transcription response: %w", err)
		}
		return strings.TrimSpace(out.Text), nil
	}
	return strings.TrimSpace(string(trimmed)), nil
}

// retryAfterError wraps a retryable failure with the server's Retry-After
// hint, so backoff can honor it instead of guessing.
type retryAfterError struct {
	err   error
	after time.Duration
}

func (e *retryAfterError) Error() string { return e.err.Error() }
func (e *retryAfterError) Unwrap() error { return e.err }

// maxRetryAfter bounds how long a server can park us. A hostile or misconfigured
// Retry-After of a day must not hang a conversion.
const maxRetryAfter = 30 * time.Second

// parseRetryAfter understands both forms of the header: delta-seconds and an
// HTTP-date. An unparseable value yields 0, meaning "use plain backoff".
func parseRetryAfter(v string) time.Duration {
	v = strings.TrimSpace(v)
	if v == "" {
		return 0
	}
	if secs, err := strconv.Atoi(v); err == nil {
		if secs < 0 {
			return 0
		}
		return capDuration(time.Duration(secs) * time.Second)
	}
	if when, err := http.ParseTime(v); err == nil {
		d := time.Until(when)
		if d <= 0 {
			return 0
		}
		return capDuration(d)
	}
	return 0
}

func capDuration(d time.Duration) time.Duration {
	if d > maxRetryAfter {
		return maxRetryAfter
	}
	return d
}

// backoff returns how long to wait before attempt n (1-based). Retry-After
// wins when the server sent one; otherwise it is exponential with jitter, so a
// fleet of clients retrying a recovered provider does not stampede it.
func backoff(attempt int, lastErr error) time.Duration {
	var ra *retryAfterError
	if errors.As(lastErr, &ra) && ra.after > 0 {
		return ra.after
	}
	d := time.Duration(1<<uint(attempt-1)) * 200 * time.Millisecond
	if d > 8*time.Second {
		d = 8 * time.Second
	}
	return d + time.Duration(rand.Int63n(int64(d/2)+1)) //nolint:gosec // jitter, not crypto
}

// sleepCtx waits for d, or returns early if ctx is done. Every wait in the
// retry loop goes through it so a cancelled conversion stops immediately
// instead of finishing its backoff first.
func sleepCtx(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return ctx.Err()
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// isLocalEndpoint reports whether the base URL points at this machine. A local
// whisper server needs no credential, so requiring one there would be a false
// barrier — but anything remote must be authenticated deliberately.
func isLocalEndpoint(base string) bool {
	u, err := url.Parse(base)
	if err != nil {
		return false
	}
	host := u.Hostname()
	return host == "localhost" || host == "127.0.0.1" || host == "::1" || host == "0.0.0.0"
}

// mimeExt maps the media types anymd's audio converter recognises to the file
// extension the endpoint expects to see on the uploaded part.
var mimeExt = map[string]string{
	"audio/mpeg":   ".mp3",
	"audio/mp3":    ".mp3",
	"audio/mpga":   ".mp3",
	"audio/wav":    ".wav",
	"audio/x-wav":  ".wav",
	"audio/wave":   ".wav",
	"audio/mp4":    ".m4a",
	"audio/x-m4a":  ".m4a",
	"video/mp4":    ".mp4",
	"audio/flac":   ".flac",
	"audio/x-flac": ".flac",
	"audio/ogg":    ".ogg",
	"audio/opus":   ".ogg",
	"audio/webm":   ".webm",
	"video/webm":   ".webm",
}

// extForMime picks the upload filename's extension. Unknown types fall back to
// .mp3 rather than to nothing: an extensionless part is rejected outright by
// several servers, while a mislabelled one is still sniffed and decoded.
func extForMime(mime string) string {
	m := strings.ToLower(strings.TrimSpace(mime))
	if i := strings.IndexByte(m, ';'); i >= 0 {
		m = strings.TrimSpace(m[:i])
	}
	if ext, ok := mimeExt[m]; ok {
		return ext
	}
	return ".mp3"
}

// truncate bounds an error body so a server cannot write a megabyte into our
// error string.
func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
