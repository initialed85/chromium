package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/chromedp/cdproto/page"
	"github.com/chromedp/chromedp"
	"github.com/chromedp/chromedp/remote"
)

type config struct {
	port          string
	execPath      string
	cdpPort       string
	cdpBase       string
	timeout       time.Duration
	maxConcurrent int
	version       string
	startedAt     time.Time
	browserCtx    context.Context
	// glRenderer is what the browser reported for ANGLE at boot. It is logged and
	// served on GET / because it is the only way to tell a GPU backend that landed
	// from one that was silently refused.
	glRenderer string

	mu       sync.Mutex
	requests int
	slots    chan struct{}
}

// rendererProbe asks a page which GL stack ANGLE is drawing with. UNMASKED_RENDERER
// names the backend and the device behind it, so it distinguishes SwiftShader from
// a real GPU. Each context type needs a canvas of its own: ask one canvas for webgl
// and then for webgl2 and the second answer is null, which reads exactly like the
// GPU being unavailable.
const rendererProbe = `(() => {
  for (const type of ['webgl2', 'webgl']) {
    const gl = document.createElement('canvas').getContext(type);
    if (!gl) continue;
    const ext = gl.getExtension('WEBGL_debug_renderer_info');
    if (!ext) return type + ' without WEBGL_debug_renderer_info';
    return gl.getParameter(ext.UNMASKED_RENDERER_WEBGL);
  }
  return 'no GL context';
})()`

// chromeFlag is one switch from CHROME_EXTRA_FLAGS.
type chromeFlag struct {
	name  string
	value string // empty means a bare switch, which is a boolean flag set to true
}

// parseChromeFlags reads CHROME_EXTRA_FLAGS. Flags are added, not overridden:
// Chrome keeps the first copy of a repeated switch (measured - `--use-angle=swiftshader
// --use-angle=vulkan` ends on swiftshader), and every flag this service passes itself
// comes before these.
func parseChromeFlags(raw string) []chromeFlag {
	flags := []chromeFlag{}
	for _, field := range strings.Fields(raw) {
		name, value, hasValue := strings.Cut(strings.TrimLeft(field, "-"), "=")
		if name == "" {
			continue
		}
		if !hasValue {
			value = ""
		}
		flags = append(flags, chromeFlag{name: name, value: value})
	}
	return flags
}

func (f chromeFlag) option() chromedp.ExecAllocatorOption {
	if f.value == "" {
		return chromedp.Flag(f.name, true)
	}
	return chromedp.Flag(f.name, f.value)
}

func main() {
	cfg := &config{
		port:          env("PORT", "8080"),
		execPath:      env("CHROME_EXEC_PATH", "/headless-shell/headless-shell"),
		cdpPort:       env("CDP_PORT", "9222"),
		cdpBase:       "http://127.0.0.1:" + env("CDP_PORT", "9222"),
		timeout:       time.Duration(atoi(env("REQUEST_TIMEOUT_SECONDS", "30"), 30)) * time.Second,
		maxConcurrent: atoi(env("MAX_CONCURRENT", "4"), 4),
		version:       env("CHROMIUM_VERSION", "dev"),
		startedAt:     time.Now(),
		slots:         make(chan struct{}, atoi(env("MAX_CONCURRENT", "4"), 4)),
	}

	// The browser is allocated once, on a context that outlives every request.
	// Per-request contexts are children of it, so cancelling one closes only its
	// tab. A timeout on the context that first allocates the browser would kill
	// the browser itself.
	//
	// CHROME_EXTRA_FLAGS is how the draw backend is chosen, and it is empty by
	// default: with nothing asked for, the browser draws with SwiftShader and needs
	// no display. A hardware GPU needs more than flags (hack/README-gpu.md), and the
	// flags are not evidence that the backend landed - the page is.
	extra := parseChromeFlags(env("CHROME_EXTRA_FLAGS", ""))
	if len(extra) > 0 {
		log.Printf("chrome extra flags: %s", env("CHROME_EXTRA_FLAGS", ""))
	}

	browserCtx, stopBrowser, renderer, err := bootBrowser(cfg, extra)
	if err != nil {
		log.Fatalf("could not start browser at %s: %v", cfg.execPath, err)
	}
	if renderer == noGLContext && len(extra) > 0 {
		// Measured: a Vulkan backend with no display to open hands back no GL context
		// at all, and Chrome does not fall back to SwiftShader even with
		// --enable-unsafe-swiftshader. Pages drawn in software beat pages with dead
		// WebGL, so drop the extra flags and start a second browser.
		log.Printf("warning: %q produced %q rather than a GPU, restarting with no extra flags", env("CHROME_EXTRA_FLAGS", ""), renderer)
		stopBrowser()
		browserCtx, stopBrowser, renderer, err = bootBrowser(cfg, nil)
		if err != nil {
			log.Fatalf("could not start browser at %s: %v", cfg.execPath, err)
		}
	}
	defer stopBrowser()

	cfg.browserCtx = browserCtx
	cfg.glRenderer = renderer
	log.Printf("browser ready (%s, cdp 127.0.0.1:%s, drawing with %s)", cfg.version, cfg.cdpPort, renderer)

	mux := http.NewServeMux()
	mux.HandleFunc("GET /", cfg.handleIndex)
	mux.HandleFunc("GET /healthz", cfg.handleHealth)
	mux.HandleFunc("POST /render", cfg.handleRender)
	mux.HandleFunc("POST /screenshot", cfg.handleScreenshot)
	mux.HandleFunc("POST /pdf", cfg.handlePDF)
	mux.HandleFunc("POST /eval", cfg.handleEval)
	mux.HandleFunc("GET /json/version", cfg.handleCDPJSON)
	mux.HandleFunc("GET /json/list", cfg.handleCDPJSON)
	// Chrome requires PUT for /json/new.
	mux.HandleFunc("PUT /json/new", cfg.handleCDPJSON)
	mux.HandleFunc("GET /cdp/", cfg.handleCDPProxy)

	server := &http.Server{Addr: ":" + cfg.port, Handler: mux, ReadHeaderTimeout: 10 * time.Second}
	if err := server.ListenAndServe(); err != nil {
		log.Fatal(err)
	}
}

// noGLContext is what rendererProbe answers when no canvas got a context at all. It
// must match the string inside rendererProbe.
const noGLContext = "no GL context"

// allocOptions is every flag the service passes itself. These come before
// CHROME_EXTRA_FLAGS, and Chrome keeps the first copy of a repeated switch, so a
// flag named here cannot be overridden from the environment - it has to be removed
// here instead.
func allocOptions(cfg *config) []chromedp.ExecAllocatorOption {
	return []chromedp.ExecAllocatorOption{
		chromedp.ExecPath(cfg.execPath),
		chromedp.NoSandbox,
		chromedp.Headless,
		chromedp.Flag("remote-debugging-address", "127.0.0.1"),
		chromedp.Flag("remote-debugging-port", cfg.cdpPort),
		// Chrome rejects a websocket upgrade whose Origin is not localhost,
		// and it rejects a Host header that is not an IP or localhost. Both are
		// rewritten by the /cdp/ proxy below.
		chromedp.Flag("remote-allow-origins", "*"),
		remote.WebSocket,
	}
}

// bootBrowser allocates a browser and answers with the context that owns it, the way
// to shut it down, and what the page says is drawing. Run gives browserCtx the
// browser, and every tab below inherits it.
func bootBrowser(cfg *config, extra []chromeFlag) (context.Context, func(), string, error) {
	opts := allocOptions(cfg)
	for _, flag := range extra {
		opts = append(opts, flag.option())
	}
	allocCtx, allocCancel := chromedp.NewExecAllocator(context.Background(), opts...)
	browserCtx, browserCancel := chromedp.NewContext(allocCtx)
	stop := func() { browserCancel(); allocCancel() }

	if _, err := chromedp.Run(browserCtx, chromedp.Title()); err != nil {
		stop()
		return nil, func() {}, "", fmt.Errorf("could not start browser at %s: %w", cfg.execPath, err)
	}

	renderer, err := chromedp.Run(browserCtx, chromedp.Evaluate[string](rendererProbe))
	if err != nil {
		// A browser that answers for Title but not for the probe is still a browser;
		// the renderer string is diagnostic, not a precondition.
		log.Printf("could not read the GL renderer: %v", err)
		renderer = "unknown"
	}
	return browserCtx, stop, renderer, nil
}

func env(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

func atoi(s string, fallback int) int {
	v, err := strconv.Atoi(s)
	if err != nil || v <= 0 {
		return fallback
	}
	return v
}

func (c *config) count() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.requests++
	return c.requests
}

func (c *config) acquire() error {
	c.count()

	select {
	case c.slots <- struct{}{}:
		return nil
	default:
		return fmt.Errorf("too many concurrent requests (max %d)", c.maxConcurrent)
	}
}

func (c *config) release() { <-c.slots }

// tab validates the posted url and returns a chromedp context bound to its own
// tab on the shared browser.
func (c *config) tab(r *http.Request) (context.Context, context.CancelFunc, error) {
	target, err := url.Parse(r.FormValue("url"))
	if err != nil || !target.IsAbs() || (target.Scheme != "http" && target.Scheme != "https") {
		return nil, nil, fmt.Errorf("post 'url' with an absolute http or https url")
	}

	// The tab must be a child of the context that owns the browser. The request's
	// own context is not one, and a chromedp context whose parent has no browser
	// yet allocates a browser of its own: that second headless shell per request
	// leaks its zygotes, gpu-process, utility and renderer as defunct processes,
	// because they are reparented to this binary (PID 1) and Go never waits them.
	ctx, cancel := context.WithTimeout(c.browserCtx, c.timeout)
	tabCtx, cancelTab := chromedp.NewContext(ctx)

	// Nothing in the tab's chain listens to the request, so a client that hangs
	// up cancels nothing on its own; close the tab when the request ends.
	context.AfterFunc(r.Context(), func() { cancelTab() })

	return tabCtx, func() {
		cancelTab()
		cancel()
	}, nil
}

func (c *config) handleIndex(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}

	requests := c.count()

	browser := c.cdpVersion()
	if browser != nil {
		rewriteWS(browser, c.externalWSBase(r), c.cdpPort)
	}

	writeJSON(w, map[string]any{
		"service":        "chromium",
		"version":        c.version,
		"browser":        browser,
		"endpoints":      []string{"/render", "/screenshot", "/pdf", "/eval", "/json/version", "/json/list", "/json/new", "/cdp/"},
		"websocket":      c.externalWSBase(r) + "/devtools/browser",
		"timeout_s":      int(c.timeout.Seconds()),
		"max_concurrent": c.maxConcurrent,
		"gl_renderer":    c.glRenderer,
		"uptime_s":       time.Since(c.startedAt).Seconds(),
		"requests":       requests,
	})
}

func (c *config) handleHealth(w http.ResponseWriter, r *http.Request) {
	v := c.cdpVersion()
	if v == nil {
		http.Error(w, `{"ok":false,"error":"browser not answering on cdp"}`, http.StatusServiceUnavailable)
		return
	}
	writeJSON(w, map[string]any{"ok": true, "browser": v["Browser"]})
}

func (c *config) handleRender(w http.ResponseWriter, r *http.Request) {
	if err := c.acquire(); err != nil {
		http.Error(w, err.Error(), http.StatusTooManyRequests)
		return
	}
	defer c.release()

	tabCtx, cancel, err := c.tab(r)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	defer cancel()

	resp, err := chromedp.Run(tabCtx, chromedp.NavigateResponse(r.FormValue("url")))
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	title, _ := chromedp.Run(tabCtx, chromedp.Title())
	text, _ := chromedp.Run(tabCtx, chromedp.Evaluate[string](`document.body ? document.body.innerText : ""`))

	writeJSON(w, map[string]any{
		"url":    r.FormValue("url"),
		"status": resp.Status,
		"title":  title,
		"text":   text,
	})
}

func (c *config) handleScreenshot(w http.ResponseWriter, r *http.Request) {
	if err := c.acquire(); err != nil {
		http.Error(w, err.Error(), http.StatusTooManyRequests)
		return
	}
	defer c.release()

	tabCtx, cancel, err := c.tab(r)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	defer cancel()

	if err := chromedp.Do(tabCtx, chromedp.Navigate(r.FormValue("url"))); err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}

	png, err := chromedp.Run(tabCtx, chromedp.CaptureScreenshot())
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}

	w.Header().Set("Content-Type", "image/png")
	w.Write(png)
}

func (c *config) handlePDF(w http.ResponseWriter, r *http.Request) {
	if err := c.acquire(); err != nil {
		http.Error(w, err.Error(), http.StatusTooManyRequests)
		return
	}
	defer c.release()

	tabCtx, cancel, err := c.tab(r)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	defer cancel()

	printBackground := true
	res, err := chromedp.Call(tabCtx, page.PrintToPDF, page.PrintToPDFParams{PrintBackground: &printBackground})
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}

	w.Header().Set("Content-Type", "application/pdf")
	w.Write(res.Data)
}

func (c *config) handleEval(w http.ResponseWriter, r *http.Request) {
	if err := c.acquire(); err != nil {
		http.Error(w, err.Error(), http.StatusTooManyRequests)
		return
	}
	defer c.release()

	expression := r.FormValue("expression")
	if expression == "" {
		http.Error(w, "post 'expression'", http.StatusBadRequest)
		return
	}

	tabCtx, cancel, err := c.tab(r)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	defer cancel()

	if err := chromedp.Do(tabCtx, chromedp.Navigate(r.FormValue("url"))); err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}

	value, err := chromedp.Run(tabCtx, chromedp.Evaluate[any](expression))
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}

	writeJSON(w, map[string]any{"url": r.FormValue("url"), "result": value})
}

func (c *config) handleCDPJSON(w http.ResponseWriter, r *http.Request) {
	if err := c.acquire(); err != nil {
		http.Error(w, err.Error(), http.StatusTooManyRequests)
		return
	}
	defer c.release()

	body, err := c.cdpFetch(r.Method, r.URL.Path)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}

	var v any
	if err := json.Unmarshal(body, &v); err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	rewriteWS(v, c.externalWSBase(r), c.cdpPort)
	writeJSON(w, v)
}

func (c *config) handleCDPProxy(w http.ResponseWriter, r *http.Request) {
	if err := c.acquire(); err != nil {
		http.Error(w, err.Error(), http.StatusTooManyRequests)
		return
	}
	defer c.release()

	proxy := &httputil.ReverseProxy{Director: func(req *http.Request) {
		req.URL.Scheme = "http"
		req.URL.Host = "127.0.0.1:" + c.cdpPort
		req.Host = "127.0.0.1:" + c.cdpPort
		req.URL.Path = strings.TrimPrefix(req.URL.Path, "/cdp")
	}}
	proxy.ServeHTTP(w, r)
}

func (c *config) cdpFetch(method, path string) ([]byte, error) {
	client := &http.Client{Timeout: 10 * time.Second}
	req, err := http.NewRequest(method, c.cdpBase+path, nil)
	if err != nil {
		return nil, err
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	return io.ReadAll(resp.Body)
}

func (c *config) cdpVersion() map[string]any {
	body, err := c.cdpFetch("GET", "/json/version")
	if err != nil {
		return nil
	}
	var v map[string]any
	if json.Unmarshal(body, &v) != nil {
		return nil
	}
	return v
}

func (c *config) externalWSBase(r *http.Request) string {
	scheme := "ws"
	if r.TLS != nil || r.Header.Get("X-Forwarded-Proto") == "https" {
		scheme = "wss"
	}
	return scheme + "://" + r.Host + "/cdp"
}

// rewriteWS rewrites the webSocketDebuggerUrl of a CDP discovery response so it
// points back through this service instead of the loopback CDP port.
func rewriteWS(v any, base, cdpPort string) {
	prefix := "ws://127.0.0.1:" + cdpPort
	switch t := v.(type) {
	case map[string]any:
		if u, ok := t["webSocketDebuggerUrl"].(string); ok {
			t["webSocketDebuggerUrl"] = base + strings.TrimPrefix(u, prefix)
		}
	case []any:
		for _, item := range t {
			rewriteWS(item, base, cdpPort)
		}
	}
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}
