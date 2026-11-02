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

func TestParseChromeFlags(t *testing.T) {
	cases := []struct {
		raw  string
		want []chromeFlag
	}{
		{"", []chromeFlag{}},
		{"--enable-gpu", []chromeFlag{{name: "enable-gpu", value: ""}}},
		{"--use-angle=vulkan", []chromeFlag{{name: "use-angle", value: "vulkan"}}},
		{
			"  --enable-gpu   --use-angle=vulkan\t--enable-unsafe-swiftshader ",
			[]chromeFlag{
				{name: "enable-gpu", value: ""},
				{name: "use-angle", value: "vulkan"},
				{name: "enable-unsafe-swiftshader", value: ""},
			},
		},
		{"--no-disabled-features=Foo,Bar", []chromeFlag{{name: "no-disabled-features", value: "Foo,Bar"}}},
		{"-", []chromeFlag{}},
	}

	for _, c := range cases {
		got := parseChromeFlags(c.raw)
		if len(got) != len(c.want) {
			t.Fatalf("parseChromeFlags(%q): got %d flags %v, want %d", c.raw, len(got), got, len(c.want))
		}
		for i := range got {
			if got[i] != c.want[i] {
				t.Fatalf("parseChromeFlags(%q): flag %d is %+v, want %+v", c.raw, i, got[i], c.want[i])
			}
		}
		for _, f := range got {
			// A flag with no value is a switch, not a switch with an empty value;
			// chromedp only emits `--name` for the first.
			if f.option() == nil {
				t.Fatalf("parseChromeFlags(%q): %+v has no allocator option", c.raw, f)
			}
		}
	}
}
