package llm

import (
	"context"
	"errors"
	"fmt"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/muthuishere/anymd"
)

// The whole point of this file: the type must satisfy the core interface.
var _ anymd.Transcriber = (*Transcriber)(nil)

// capturedUpload is what the fake endpoint saw, so a test can assert the wire
// shape rather than trusting the client.
type capturedUpload struct {
	model    string
	filename string
	fileLen  int
	auth     string
	headers  http.Header
}

// parseUpload decodes a multipart transcription request.
func parseUpload(t *testing.T, r *http.Request) capturedUpload {
	t.Helper()
	ct := r.Header.Get("Content-Type")
	mt, params, err := mime.ParseMediaType(ct)
	if err != nil || mt != "multipart/form-data" {
		t.Fatalf("Content-Type = %q, want multipart/form-data (err=%v)", ct, err)
	}
	got := capturedUpload{auth: r.Header.Get("Authorization"), headers: r.Header.Clone()}
	mr := multipart.NewReader(r.Body, params["boundary"])
	for {
		p, err := mr.NextPart()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("reading multipart: %v", err)
		}
		body, _ := io.ReadAll(p)
		switch p.FormName() {
		case "file":
			got.filename = p.FileName()
			got.fileLen = len(body)
		case "model":
			got.model = string(body)
		}
	}
	return got
}

func testTranscriber(t *testing.T, srv *httptest.Server, model string) *Transcriber {
	t.Helper()
	return NewTranscriber(Config{
		BaseURL:   srv.URL,
		APIKey:    "test-key",
		Retries:   2,
		TimeoutMs: 5000,
	}, model)
}

// TestTranscribeMultipartShape is the wire contract: a "file" part whose
// filename carries an extension matching the mime, plus a "model" field.
func TestTranscribeMultipartShape(t *testing.T) {
	var got capturedUpload
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Errorf("method = %s, want POST", r.Method)
		}
		if r.URL.Path != "/audio/transcriptions" {
			t.Errorf("path = %q, want /audio/transcriptions", r.URL.Path)
		}
		got = parseUpload(t, r)
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"text":"  hello there  "}`)
	}))
	defer srv.Close()

	tr := testTranscriber(t, srv, "")
	out, err := tr.Transcribe(context.Background(), []byte("fake-wav-bytes"), "audio/wav")
	if err != nil {
		t.Fatalf("Transcribe: %v", err)
	}
	if out != "hello there" {
		t.Errorf("text = %q, want %q", out, "hello there")
	}
	if got.model != DefaultTranscribeModel {
		t.Errorf("model field = %q, want %q", got.model, DefaultTranscribeModel)
	}
	if ext := filepath.Ext(got.filename); ext != ".wav" {
		t.Errorf("file part filename = %q, want a .wav extension", got.filename)
	}
	if got.fileLen != len("fake-wav-bytes") {
		t.Errorf("file part length = %d, want %d", got.fileLen, len("fake-wav-bytes"))
	}
	if got.auth != "Bearer test-key" {
		t.Errorf("Authorization = %q, want the configured key", got.auth)
	}
}

// TestTranscribeModelOverrideAndExtensions checks the constructor's model
// argument wins and each mime maps to the extension a server dispatches on.
func TestTranscribeModelOverrideAndExtensions(t *testing.T) {
	cases := []struct{ mime, wantExt string }{
		{"audio/mpeg", ".mp3"},
		{"audio/wav", ".wav"},
		{"audio/mp4", ".m4a"},
		{"audio/flac", ".flac"},
		{"audio/ogg", ".ogg"},
		{"audio/webm", ".webm"},
		{"audio/mpeg; codecs=mp3", ".mp3"},
		{"", ".mp3"}, // unknown must still carry SOME extension
	}
	for _, tc := range cases {
		t.Run(tc.mime, func(t *testing.T) {
			var got capturedUpload
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				got = parseUpload(t, r)
				fmt.Fprint(w, `{"text":"ok"}`)
			}))
			defer srv.Close()

			tr := testTranscriber(t, srv, "whisper-large-v3")
			if _, err := tr.Transcribe(context.Background(), []byte("x"), tc.mime); err != nil {
				t.Fatalf("Transcribe: %v", err)
			}
			if ext := filepath.Ext(got.filename); ext != tc.wantExt {
				t.Errorf("filename %q ext = %q, want %q", got.filename, ext, tc.wantExt)
			}
			if got.model != "whisper-large-v3" {
				t.Errorf("model = %q, want the constructor override", got.model)
			}
		})
	}
}

// TestTranscribeExtraHeaders: gateway headers ride along on every attempt.
func TestTranscribeExtraHeaders(t *testing.T) {
	var got capturedUpload
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = parseUpload(t, r)
		fmt.Fprint(w, `{"text":"ok"}`)
	}))
	defer srv.Close()

	tr := NewTranscriber(Config{
		BaseURL: srv.URL,
		APIKey:  "k",
		Headers: map[string]string{"X-Tenant": "acme"},
	}, "")
	if _, err := tr.Transcribe(context.Background(), []byte("x"), "audio/mpeg"); err != nil {
		t.Fatalf("Transcribe: %v", err)
	}
	if v := got.headers.Get("X-Tenant"); v != "acme" {
		t.Errorf("X-Tenant = %q, want acme", v)
	}
}

// TestTranscribePlainTextResponse: response_format=text servers return a bare
// body, not JSON.
func TestTranscribePlainTextResponse(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, "just words\n")
	}))
	defer srv.Close()

	out, err := testTranscriber(t, srv, "").Transcribe(context.Background(), []byte("x"), "audio/mpeg")
	if err != nil {
		t.Fatalf("Transcribe: %v", err)
	}
	if out != "just words" {
		t.Errorf("text = %q", out)
	}
}

// TestTranscribeRetriesOn429 proves a rate limit is transient, not fatal.
func TestTranscribeRetriesOn429(t *testing.T) {
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if atomic.AddInt32(&calls, 1) == 1 {
			w.WriteHeader(http.StatusTooManyRequests)
			fmt.Fprint(w, `{"error":"slow down"}`)
			return
		}
		fmt.Fprint(w, `{"text":"second time lucky"}`)
	}))
	defer srv.Close()

	out, err := testTranscriber(t, srv, "").Transcribe(context.Background(), []byte("x"), "audio/mpeg")
	if err != nil {
		t.Fatalf("Transcribe: %v", err)
	}
	if out != "second time lucky" {
		t.Errorf("text = %q", out)
	}
	if got := atomic.LoadInt32(&calls); got != 2 {
		t.Errorf("calls = %d, want 2", got)
	}
}

// TestTranscribeHonorsRetryAfter: the wait must be at least what the server
// asked for, not the (much shorter) default backoff.
func TestTranscribeHonorsRetryAfter(t *testing.T) {
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if atomic.AddInt32(&calls, 1) == 1 {
			w.Header().Set("Retry-After", "1")
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		fmt.Fprint(w, `{"text":"ok"}`)
	}))
	defer srv.Close()

	start := time.Now()
	if _, err := testTranscriber(t, srv, "").Transcribe(context.Background(), []byte("x"), "audio/mpeg"); err != nil {
		t.Fatalf("Transcribe: %v", err)
	}
	if elapsed := time.Since(start); elapsed < time.Second {
		t.Errorf("returned after %v, want at least the 1s Retry-After", elapsed)
	}
}

// TestParseRetryAfter covers both header forms and the hostile values.
func TestParseRetryAfter(t *testing.T) {
	if got := parseRetryAfter("2"); got != 2*time.Second {
		t.Errorf("delta-seconds: got %v", got)
	}
	if got := parseRetryAfter(""); got != 0 {
		t.Errorf("empty: got %v", got)
	}
	if got := parseRetryAfter("not-a-date"); got != 0 {
		t.Errorf("garbage: got %v", got)
	}
	if got := parseRetryAfter("-5"); got != 0 {
		t.Errorf("negative: got %v", got)
	}
	if got := parseRetryAfter("100000"); got != maxRetryAfter {
		t.Errorf("absurd value must clamp, got %v", got)
	}
	past := time.Now().Add(-time.Hour).UTC().Format(http.TimeFormat)
	if got := parseRetryAfter(past); got != 0 {
		t.Errorf("past date: got %v", got)
	}
	future := time.Now().Add(2 * time.Second).UTC().Format(http.TimeFormat)
	if got := parseRetryAfter(future); got <= 0 || got > maxRetryAfter {
		t.Errorf("future date: got %v", got)
	}
}

// TestTranscribeExhaustsRetriesOn500 returns a typed error naming the status.
func TestTranscribeExhaustsRetriesOn500(t *testing.T) {
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
		w.WriteHeader(http.StatusInternalServerError)
		fmt.Fprint(w, "boom")
	}))
	defer srv.Close()

	tr := NewTranscriber(Config{BaseURL: srv.URL, APIKey: "k", Retries: 2}, "")
	_, err := tr.Transcribe(context.Background(), []byte("x"), "audio/mpeg")
	if err == nil {
		t.Fatal("want an error")
	}
	var te *TranscribeError
	if !errors.As(err, &te) {
		t.Fatalf("error %v is not a *TranscribeError", err)
	}
	if te.Status != http.StatusInternalServerError {
		t.Errorf("status = %d", te.Status)
	}
	if !strings.Contains(te.Body, "boom") {
		t.Errorf("body = %q, want the server's message", te.Body)
	}
	if got := atomic.LoadInt32(&calls); got != 3 {
		t.Errorf("calls = %d, want 1 + 2 retries", got)
	}
}

// TestTranscribeDoesNotRetry4xx: a bad request is our fault; retrying it only
// burns quota.
func TestTranscribeDoesNotRetry4xx(t *testing.T) {
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
		w.WriteHeader(http.StatusBadRequest)
		fmt.Fprint(w, `{"error":"unsupported file"}`)
	}))
	defer srv.Close()

	tr := NewTranscriber(Config{BaseURL: srv.URL, APIKey: "k", Retries: 3}, "")
	_, err := tr.Transcribe(context.Background(), []byte("x"), "audio/mpeg")
	var te *TranscribeError
	if !errors.As(err, &te) || te.Status != http.StatusBadRequest {
		t.Fatalf("err = %v, want a 400 TranscribeError", err)
	}
	if got := atomic.LoadInt32(&calls); got != 1 {
		t.Errorf("calls = %d, want exactly 1 (no retry on 4xx)", got)
	}
}

// TestTranscribeEmptyTextIsAnError: a 200 with no transcript is a failure, not
// a successful conversion of nothing.
func TestTranscribeEmptyTextIsAnError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"text":"   "}`)
	}))
	defer srv.Close()

	if _, err := testTranscriber(t, srv, "").Transcribe(context.Background(), []byte("x"), "audio/mpeg"); err == nil {
		t.Fatal("want an error for an empty transcript")
	}
}

// TestTranscribeContextCancelled: ctx wins over an in-flight request.
func TestTranscribeContextCancelled(t *testing.T) {
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-release
		fmt.Fprint(w, `{"text":"too late"}`)
	}))
	defer func() { close(release); srv.Close() }()

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(50 * time.Millisecond)
		cancel()
	}()
	_, err := testTranscriber(t, srv, "").Transcribe(ctx, []byte("x"), "audio/mpeg")
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
}

// TestTranscribeContextCancelledDuringBackoff: the retry wait must be
// interruptible, or a cancelled conversion still pays for the sleep.
func TestTranscribeContextCancelledDuringBackoff(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Retry-After", "30")
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	defer srv.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	start := time.Now()
	_, err := testTranscriber(t, srv, "").Transcribe(ctx, []byte("x"), "audio/mpeg")
	if err == nil {
		t.Fatal("want an error")
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Errorf("waited %v: backoff did not honor ctx", elapsed)
	}
}

// TestTranscribeOversizeRejected: refuse locally rather than upload 200 MiB to
// be told no.
func TestTranscribeOversizeRejected(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("oversize audio must not reach the network")
	}))
	defer srv.Close()

	big := make([]byte, MaxAudioBytes+1)
	_, err := testTranscriber(t, srv, "").Transcribe(context.Background(), big, "audio/mpeg")
	if err == nil || !strings.Contains(err.Error(), "too large") {
		t.Fatalf("err = %v, want a size error", err)
	}
}

// TestTranscribeEmptyAudioRejected.
func TestTranscribeEmptyAudioRejected(t *testing.T) {
	tr := NewTranscriber(Config{BaseURL: "http://127.0.0.1:1", APIKey: "k"}, "")
	if _, err := tr.Transcribe(context.Background(), nil, "audio/mpeg"); err == nil {
		t.Fatal("want an error for empty audio")
	}
}

// TestTranscribeMissingKeyRemote: no key against a remote endpoint must fail
// fast, naming the ENVIRONMENT VARIABLE and never a value.
func TestTranscribeMissingKeyRemote(t *testing.T) {
	for _, name := range transcribeAPIKeyEnv {
		t.Setenv(name, "")
	}
	tr := NewTranscriber(Config{BaseURL: "https://api.example.com/v1"}, "")
	_, err := tr.Transcribe(context.Background(), []byte("x"), "audio/mpeg")
	if !errors.Is(err, ErrNoAPIKey) {
		t.Fatalf("err = %v, want ErrNoAPIKey", err)
	}
	if !strings.Contains(err.Error(), "OPENAI_API_KEY") {
		t.Errorf("error %q should name the environment variable", err)
	}
}

// TestTranscribeMissingKeyLocalIsAllowed: a local whisper server needs no
// credential, so requiring one would be a false barrier.
func TestTranscribeMissingKeyLocalIsAllowed(t *testing.T) {
	for _, name := range transcribeAPIKeyEnv {
		t.Setenv(name, "")
	}
	var sawAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sawAuth = r.Header.Get("Authorization")
		fmt.Fprint(w, `{"text":"local"}`)
	}))
	defer srv.Close()

	tr := NewTranscriber(Config{BaseURL: srv.URL}, "") // httptest is 127.0.0.1
	out, err := tr.Transcribe(context.Background(), []byte("x"), "audio/mpeg")
	if err != nil {
		t.Fatalf("Transcribe: %v", err)
	}
	if out != "local" {
		t.Errorf("text = %q", out)
	}
	if sawAuth != "" {
		t.Errorf("Authorization = %q, want none", sawAuth)
	}
}

// TestTranscribeKeyFromEnvironment: leaving Config.APIKey empty must pick the
// key up from the environment, the way the Describer does.
func TestTranscribeKeyFromEnvironment(t *testing.T) {
	t.Setenv("OPENAI_API_KEY", "env-key")
	var auth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auth = r.Header.Get("Authorization")
		fmt.Fprint(w, `{"text":"ok"}`)
	}))
	defer srv.Close()

	tr := NewTranscriber(Config{BaseURL: srv.URL}, "")
	if _, err := tr.Transcribe(context.Background(), []byte("x"), "audio/mpeg"); err != nil {
		t.Fatalf("Transcribe: %v", err)
	}
	if auth != "Bearer env-key" {
		t.Errorf("Authorization = %q, want the environment key", auth)
	}
}

// TestIsLocalEndpoint.
func TestIsLocalEndpoint(t *testing.T) {
	local := []string{"http://localhost:8080/v1", "http://127.0.0.1:1234/v1", "http://[::1]:9/v1"}
	remote := []string{"https://api.openai.com/v1", "https://example.com", "::::"}
	for _, u := range local {
		if !isLocalEndpoint(u) {
			t.Errorf("%s should be local", u)
		}
	}
	for _, u := range remote {
		if isLocalEndpoint(u) {
			t.Errorf("%s should not be local", u)
		}
	}
}

// TestTranscribeBadJSONNotRetried: a malformed 200 is a broken server, and
// re-uploading the audio will not fix it.
func TestTranscribeBadJSONNotRetried(t *testing.T) {
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
		fmt.Fprint(w, `{"text": `)
	}))
	defer srv.Close()

	if _, err := testTranscriber(t, srv, "").Transcribe(context.Background(), []byte("x"), "audio/mpeg"); err == nil {
		t.Fatal("want a decode error")
	}
	if got := atomic.LoadInt32(&calls); got != 1 {
		t.Errorf("calls = %d, want 1", got)
	}
}
