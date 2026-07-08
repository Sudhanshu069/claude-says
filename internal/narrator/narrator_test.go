package narrator

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/Sudhanshu069/claude-code-speak/internal/config"
)

// roundTripFunc adapts a func to http.RoundTripper so tests can inject canned
// responses into GeminiNarrator.http without any network access.
type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func resp(code int, body string) *http.Response {
	return &http.Response{
		StatusCode: code,
		Body:       io.NopCloser(strings.NewReader(body)),
		Header:     make(http.Header),
	}
}

// newGeminiRT builds a GeminiNarrator with the given key and a fake transport.
func newGeminiRT(apiKey string, rt roundTripFunc) *GeminiNarrator {
	return &GeminiNarrator{
		http:   &http.Client{Transport: rt},
		apiKey: apiKey,
		model:  "gemini-2.5-flash",
	}
}

const okBody = `{"candidates":[{"content":{"parts":[{"text":"Claude is fixing the bug."}]}}]}`

func TestNarrateOrErr_Success(t *testing.T) {
	t.Parallel()

	const input = "I am editing line 42 of src/foo.js to fix the crash."
	var captured *http.Request
	var capturedBody string

	rt := roundTripFunc(func(r *http.Request) (*http.Response, error) {
		captured = r
		b, _ := io.ReadAll(r.Body)
		capturedBody = string(b)
		return resp(http.StatusOK, okBody), nil
	})

	n := newGeminiRT("k", rt)
	out, err := n.NarrateOrErr(context.Background(), input)
	if err != nil {
		t.Fatalf("NarrateOrErr returned error on 200: %v", err)
	}
	if out != "Claude is fixing the bug." {
		t.Fatalf("out = %q, want rephrased text", out)
	}

	// Header carries the key (never the URL).
	if got := captured.Header.Get("x-goog-api-key"); got != "k" {
		t.Errorf("x-goog-api-key = %q, want %q", got, "k")
	}
	if got := captured.URL.String(); !strings.Contains(got, "gemini-2.5-flash:generateContent") {
		t.Errorf("URL = %q, want contains %q", got, "gemini-2.5-flash:generateContent")
	}
	if strings.Contains(captured.URL.String(), "k") && strings.Contains(captured.URL.RawQuery, "k") {
		t.Errorf("api key must not appear in URL query: %q", captured.URL.String())
	}
	// Body carries the system instruction and the exact narrate framing.
	if !strings.Contains(capturedBody, "system_instruction") {
		t.Errorf("body missing system_instruction: %q", capturedBody)
	}
	// The framing prefix + text is JSON-encoded, so the newlines are escaped.
	wantFrag := `Narrate this AI assistant output:\n\n` + input
	if !strings.Contains(capturedBody, wantFrag) {
		t.Errorf("body missing narrate framing %q in %q", wantFrag, capturedBody)
	}
}

func TestNarrate_TotalOnMissingKey(t *testing.T) {
	t.Parallel()

	const input = "some assistant text"
	// Transport must never be hit when the key is missing.
	rt := roundTripFunc(func(r *http.Request) (*http.Response, error) {
		t.Errorf("transport called with no api key: %s", r.URL)
		return resp(http.StatusOK, okBody), nil
	})
	n := newGeminiRT("", rt)

	if got := n.Narrate(context.Background(), input); got != input {
		t.Errorf("Narrate = %q, want input verbatim %q", got, input)
	}

	out, err := n.NarrateOrErr(context.Background(), input)
	if out != input {
		t.Errorf("NarrateOrErr out = %q, want input %q", out, input)
	}
	if err != errNoAPIKey {
		t.Errorf("NarrateOrErr err = %v, want errNoAPIKey", err)
	}
}

func TestNarrate_NonOK(t *testing.T) {
	t.Parallel()

	const input = "assistant output text"
	for _, code := range []int{400, 429, 500, 503} {
		rt := roundTripFunc(func(r *http.Request) (*http.Response, error) {
			return resp(code, `{"error":"boom"}`), nil
		})
		n := newGeminiRT("k", rt)

		out, err := n.NarrateOrErr(context.Background(), input)
		if out != input {
			t.Errorf("[%d] out = %q, want input verbatim", code, out)
		}
		if err == nil {
			t.Fatalf("[%d] NarrateOrErr err = nil, want gemini API error", code)
		}
		if !strings.Contains(err.Error(), "gemini API error") {
			t.Errorf("[%d] err = %q, want contains 'gemini API error'", code, err.Error())
		}
		// Narrate swallows the error and still returns input.
		if got := n.Narrate(context.Background(), input); got != input {
			t.Errorf("[%d] Narrate = %q, want input verbatim", code, got)
		}
	}
}

func TestNarrate_MalformedBody(t *testing.T) {
	t.Parallel()

	const input = "assistant output text"
	rt := roundTripFunc(func(r *http.Request) (*http.Response, error) {
		return resp(http.StatusOK, `{"candidates": [ not json`), nil
	})
	n := newGeminiRT("k", rt)

	out, err := n.NarrateOrErr(context.Background(), input)
	if out != input {
		t.Errorf("out = %q, want input verbatim on parse failure", out)
	}
	if err == nil {
		t.Errorf("err = nil, want a JSON decode error")
	}
	if got := n.Narrate(context.Background(), input); got != input {
		t.Errorf("Narrate = %q, want input verbatim", got)
	}
}

func TestNarrate_EmptyCandidates(t *testing.T) {
	t.Parallel()

	const input = "assistant output text"
	for name, body := range map[string]string{
		"no candidates": `{"candidates":[]}`,
		"empty parts":   `{"candidates":[{"content":{"parts":[]}}]}`,
	} {
		rt := roundTripFunc(func(r *http.Request) (*http.Response, error) {
			return resp(http.StatusOK, body), nil
		})
		n := newGeminiRT("k", rt)

		out, err := n.NarrateOrErr(context.Background(), input)
		if err != nil {
			t.Errorf("[%s] err = %v, want nil (empty response is not an error)", name, err)
		}
		if out != input {
			t.Errorf("[%s] out = %q, want input verbatim", name, out)
		}
	}
}

func TestValidate(t *testing.T) {
	t.Parallel()

	t.Run("no key", func(t *testing.T) {
		t.Parallel()
		n := newGeminiRT("", roundTripFunc(func(r *http.Request) (*http.Response, error) {
			t.Errorf("transport called with no key")
			return resp(http.StatusOK, okBody), nil
		}))
		if err := n.Validate(context.Background()); err != errNoAPIKey {
			t.Errorf("Validate err = %v, want errNoAPIKey", err)
		}
	})

	t.Run("success", func(t *testing.T) {
		t.Parallel()
		n := newGeminiRT("k", roundTripFunc(func(r *http.Request) (*http.Response, error) {
			return resp(http.StatusOK, okBody), nil
		}))
		if err := n.Validate(context.Background()); err != nil {
			t.Errorf("Validate err = %v, want nil", err)
		}
	})

	t.Run("empty response", func(t *testing.T) {
		t.Parallel()
		n := newGeminiRT("k", roundTripFunc(func(r *http.Request) (*http.Response, error) {
			return resp(http.StatusOK, `{"candidates":[]}`), nil
		}))
		if err := n.Validate(context.Background()); err == nil {
			t.Errorf("Validate err = nil, want empty-response error")
		}
	})

	t.Run("non-200 surfaces", func(t *testing.T) {
		t.Parallel()
		n := newGeminiRT("k", roundTripFunc(func(r *http.Request) (*http.Response, error) {
			return resp(http.StatusServiceUnavailable, `{}`), nil
		}))
		err := n.Validate(context.Background())
		if err == nil || !strings.Contains(err.Error(), "gemini API error") {
			t.Errorf("Validate err = %v, want gemini API error", err)
		}
	})
}

func TestRegistry(t *testing.T) {
	t.Parallel()

	t.Run("default gemini", func(t *testing.T) {
		t.Parallel()
		cfg := config.DefaultConfig()
		cfg.Narrator.Provider = ""
		nr, err := New(&cfg)
		if err != nil {
			t.Fatalf("New err = %v", err)
		}
		if _, ok := nr.(*GeminiNarrator); !ok {
			t.Errorf("New returned %T, want *GeminiNarrator", nr)
		}
	})

	t.Run("explicit gemini", func(t *testing.T) {
		t.Parallel()
		cfg := config.DefaultConfig()
		cfg.Narrator.Provider = "gemini"
		if _, err := New(&cfg); err != nil {
			t.Errorf("New(gemini) err = %v", err)
		}
	})

	t.Run("unknown provider", func(t *testing.T) {
		t.Parallel()
		cfg := config.DefaultConfig()
		cfg.Narrator.Provider = "nope"
		_, err := New(&cfg)
		if err == nil {
			t.Fatalf("New(nope) err = nil, want ErrUnknownNarrator")
		}
		if !strings.Contains(err.Error(), "nope") {
			t.Errorf("err = %v, want to name the unknown provider", err)
		}
		if !errors.Is(err, ErrUnknownNarrator) {
			t.Errorf("err = %v, want errors.Is ErrUnknownNarrator", err)
		}
	})

	t.Run("List", func(t *testing.T) {
		t.Parallel()
		got := List()
		if len(got) != 1 || got[0] != "gemini" {
			t.Errorf("List() = %v, want [gemini]", got)
		}
	})
}
