package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestRewriteWSTarget(t *testing.T) {
	raw := []byte(`[{"id":"t1","type":"page","webSocketDebuggerUrl":"ws://127.0.0.1:9222/devtools/page/t1"}]`)

	var v any
	if err := json.Unmarshal(raw, &v); err != nil {
		t.Fatal(err)
	}
	rewriteWS(v, "wss://chromium.dev.initialed85.cc/cdp", "9222")

	got := v.([]any)[0].(map[string]any)["webSocketDebuggerUrl"]
	want := "wss://chromium.dev.initialed85.cc/cdp/devtools/page/t1"
	if got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
}

func TestRewriteWSVersion(t *testing.T) {
	raw := []byte(`{"Browser":"HeadlessChrome/141.0","webSocketDebuggerUrl":"ws://127.0.0.1:9222/devtools/browser/abc"}`)

	var v any
	if err := json.Unmarshal(raw, &v); err != nil {
		t.Fatal(err)
	}
	rewriteWS(v, "ws://chromium.dev.initialed85.cc/cdp", "9222")

	got := v.(map[string]any)["webSocketDebuggerUrl"]
	want := "ws://chromium.dev.initialed85.cc/cdp/devtools/browser/abc"
	if got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
}

func TestCDPJSONForwardsMethodAndRewritesWS(t *testing.T) {
	var got string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.Method
		w.Write([]byte(`{"id":"t1","webSocketDebuggerUrl":"ws://127.0.0.1:9222/devtools/page/t1"}`))
	}))
	defer srv.Close()

	c := &config{cdpPort: "9222", cdpBase: srv.URL, slots: make(chan struct{}, 1)}

	req := httptest.NewRequest(http.MethodPut, "/json/new", nil)
	w := httptest.NewRecorder()
	c.handleCDPJSON(w, req)

	if got != http.MethodPut {
		t.Fatalf("method forwarded: got %q, want PUT", got)
	}

	var v map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &v); err != nil {
		t.Fatal(err)
	}
	want := "ws://example.com/cdp/devtools/page/t1"
	if v["webSocketDebuggerUrl"] != want {
		t.Fatalf("ws url rewritten: got %q, want %q", v["webSocketDebuggerUrl"], want)
	}
}
