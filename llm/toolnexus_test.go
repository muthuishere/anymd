package llm

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/muthuishere/anymd"
)

// The whole point of this package: it must satisfy the core interface.
var _ anymd.Describer = (*Client)(nil)

// fakeModel serves the OpenAI chat-completions shape and records what it got.
func fakeModel(t *testing.T, reply string, captured *map[string]any) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		if captured != nil {
			_ = json.Unmarshal(body, captured)
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"choices": []any{map[string]any{
				"message":       map[string]any{"role": "assistant", "content": reply},
				"finish_reason": "stop",
			}},
		})
	}))
}

func TestDescribeSendsImageAsDataURI(t *testing.T) {
	var got map[string]any
	srv := fakeModel(t, "A red square.", &got)
	defer srv.Close()

	c := New(Config{BaseURL: srv.URL, APIKey: "test-not-a-real-key"})
	png := []byte{0x89, 'P', 'N', 'G', 1, 2, 3}
	out, err := c.Describe(context.Background(), png, "image/png", "")
	if err != nil {
		t.Fatalf("describe: %v", err)
	}
	if out != "A red square." {
		t.Errorf("out = %q", out)
	}

	// Dig out the image part and prove the bytes survived intact.
	msgs, _ := got["messages"].([]any)
	if len(msgs) == 0 {
		t.Fatal("no messages sent")
	}
	first, _ := msgs[0].(map[string]any)
	parts, _ := first["content"].([]any)
	var url string
	for _, p := range parts {
		if m, ok := p.(map[string]any); ok && m["type"] == "image_url" {
			iu, _ := m["image_url"].(map[string]any)
			url, _ = iu["url"].(string)
		}
	}
	const prefix = "data:image/png;base64,"
	if !strings.HasPrefix(url, prefix) {
		t.Fatalf("image url = %q, want %s prefix", url, prefix)
	}
	decoded, err := base64.StdEncoding.DecodeString(strings.TrimPrefix(url, prefix))
	if err != nil {
		t.Fatalf("payload is not valid base64: %v", err)
	}
	if string(decoded) != string(png) {
		t.Errorf("round-tripped bytes = %v, want %v", decoded, png)
	}
}

func TestDescribeIncludesTheDocumentsOwnAltText(t *testing.T) {
	var got map[string]any
	srv := fakeModel(t, "ok", &got)
	defer srv.Close()

	c := New(Config{BaseURL: srv.URL, APIKey: "test-not-a-real-key"})
	if _, err := c.Describe(context.Background(), []byte{1}, "image/jpeg", "Q3 revenue chart"); err != nil {
		t.Fatalf("describe: %v", err)
	}
	blob, _ := json.Marshal(got)
	if !strings.Contains(string(blob), "Q3 revenue chart") {
		t.Error("existing alt text was discarded instead of being given to the model")
	}
}

func TestDescribeRejectsEmptyImage(t *testing.T) {
	c := New(Config{APIKey: "test-not-a-real-key"})
	if _, err := c.Describe(context.Background(), nil, "image/png", ""); err == nil {
		t.Fatal("accepted an empty image")
	}
}

func TestDescribeErrorsWhenModelSaysNothing(t *testing.T) {
	srv := fakeModel(t, "   ", nil)
	defer srv.Close()
	c := New(Config{BaseURL: srv.URL, APIKey: "test-not-a-real-key"})
	if _, err := c.Describe(context.Background(), []byte{1}, "image/png", ""); err == nil {
		t.Fatal("an empty description should be an error, not an empty caption")
	}
}

func TestDescribeHonorsContextCancellation(t *testing.T) {
	srv := fakeModel(t, "ok", nil)
	defer srv.Close()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	c := New(Config{BaseURL: srv.URL, APIKey: "test-not-a-real-key"})
	if _, err := c.Describe(ctx, []byte{1}, "image/png", ""); err == nil {
		t.Fatal("ran despite a cancelled context")
	}
}
