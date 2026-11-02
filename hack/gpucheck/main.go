// gpucheck starts headless-shell with a flag set and prints what the page
// actually sees, so a flag combo is judged by the renderer the browser reports
// instead of by which errors scroll past in a log.
//
//	CGO_ENABLED=0 go build -o /tmp/gpucheck ./hack/gpucheck
//	CHROME_FLAGS="--enable-gpu --use-angle=vulkan" /tmp/gpucheck
//
// It runs inside the pod, next to the browser, because the GPU stack that
// matters is the one the container was handed.
//
// CHROME_URL   page to probe (default about:blank)
// CHROME_REPEAT  screenshot loop, for a software/hardware timing comparison
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/chromedp/chromedp"
)

// probe asks the page about its GL stack. UNMASKED_RENDERER_WEBGL is ANGLE's
// backend plus the device behind it. Each context type needs its own canvas:
// ask a canvas for webgl and then for webgl2 and the second answer is null,
// which reads exactly like the GPU being unavailable. WebGPU is a different
// stack (Dawn), so it is asked separately.
const probe = `(() => {
  const out = {};
  for (const type of ['webgl', 'webgl2']) {
    const gl = document.createElement('canvas').getContext(type);
    if (!gl) { out[type] = 'null context'; continue; }
    const dbg = gl.getExtension('WEBGL_debug_renderer_info');
    out[type] = {
      unmasked_renderer: dbg ? gl.getParameter(dbg.UNMASKED_RENDERER_WEBGL) : 'no WEBGL_debug_renderer_info ext',
      unmasked_vendor:   dbg ? gl.getParameter(dbg.UNMASKED_VENDOR_WEBGL) : 'n/a',
      version:           gl.getParameter(gl.VERSION),
    };
  }
  out.chrome = navigator.userAgent;
  return out;
})()`

const webgpu = `(async () => {
  if (!navigator.gpu) return { webgpu: 'navigator.gpu missing' };
  const ad = await navigator.gpu.requestAdapter();
  if (!ad) return { webgpu: 'null adapter' };
  return { webgpu: { info: ad.info, features: [...ad.features].length } };
})()`

func main() {
	exec := env("CHROME_EXEC_PATH", "/headless-shell/headless-shell")
	url := env("CHROME_URL", "about:blank")
	flags := strings.Fields(env("CHROME_FLAGS", ""))
	repeat, _ := strconv.Atoi(env("CHROME_REPEAT", "0"))

	allocCtx, allocCancel := chromedp.NewExecAllocator(context.Background(),
		append([]chromedp.ExecAllocatorOption{
			chromedp.ExecPath(exec),
			chromedp.NoSandbox,
			chromedp.Headless,
			chromedp.Flag("remote-allow-origins", "*"),
		}, execFlags(flags)...)...,
	)
	defer allocCancel()

	// NewContext is what makes a chromedp context (a tab) out of the allocator
	// context; Run on the bare allocator context answers "invalid context".
	browserCtx, browserCancel := chromedp.NewContext(allocCtx)
	defer browserCancel()

	ctx, cancel := context.WithTimeout(browserCtx, 240*time.Second)
	defer cancel()

	start := time.Now()
	if _, err := chromedp.Run(ctx, chromedp.Navigate(url)); err != nil {
		fmt.Println("navigate failed:", err)
		os.Exit(1)
	}
	fmt.Printf("navigate %s in %s\n", url, time.Since(start).Round(time.Millisecond))

	gl, err := chromedp.Run(ctx, chromedp.Evaluate[map[string]any](probe))
	if err != nil {
		fmt.Println("gl probe failed:", err)
	}
	gpu, err := chromedp.Run(ctx, chromedp.Evaluate[map[string]any](webgpu, chromedp.EvalAwaitPromise))
	if err != nil {
		fmt.Println("webgpu probe failed:", err)
	}

	merged := map[string]any{}
	for k, v := range gl {
		merged[k] = v
	}
	for k, v := range gpu {
		merged[k] = v
	}
	enc, _ := json.MarshalIndent(merged, "", "  ")
	fmt.Println(string(enc))

	if repeat > 0 {
		var total time.Duration
		var bytes int
		for i := 0; i < repeat; i++ {
			t0 := time.Now()
			png, err := chromedp.Run(ctx, chromedp.CaptureScreenshot())
			if err != nil {
				fmt.Println("screenshot failed:", err)
				break
			}
			elapsed := time.Since(t0)
			total += elapsed
			bytes += len(png)
			fmt.Printf("  shot %d: %s (%d bytes)\n", i+1, elapsed.Round(time.Millisecond), len(png))
		}
		fmt.Printf("screenshots: %d in %s, mean %s, %d bytes total\n",
			repeat, total.Round(time.Millisecond), (total / time.Duration(max(repeat, 1))).Round(time.Millisecond), bytes)
	}
}

func env(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

// execFlags turns a space separated flag string into allocator options, the same
// way the service will read its CHROME_EXTRA_FLAGS.
func execFlags(flags []string) []chromedp.ExecAllocatorOption {
	opts := []chromedp.ExecAllocatorOption{}
	for _, f := range flags {
		name, value, hasValue := strings.Cut(strings.TrimLeft(f, "-"), "=")
		if hasValue {
			opts = append(opts, chromedp.Flag(name, value))
			continue
		}
		opts = append(opts, chromedp.Flag(name, true))
	}
	return opts
}
