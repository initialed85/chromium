# chromium

Headless Chromium behind a small HTTP + DevTools (CDP) API.

```
client -> chromium.dev.initialed85.cc -> Go API -> headless shell (CDP on loopback)
                                    \-> /cdp/ proxy (HTTP + websocket upgrade)
```

Live on `home-dev`, namespace `chromium`, pinned to **august-desktop**, image
`initialed85/chromium` (built on the x86 daemon, `build.sh`).

The GPU pin is for convenience — the headless shell renders with swiftshader by
default, so nothing is actually using the GPU yet.

## API

| Route | Input | Output |
| --- | --- | --- |
| `GET /` | — | service info, browser version, CDP websocket base |
| `GET /healthz` | — | `{"ok":true}` plus the browser version |
| `POST /render` | `url` | JSON: `status`, `title`, `text` |
| `POST /screenshot` | `url` | PNG |
| `POST /pdf` | `url` | PDF |
| `POST /eval` | `url`, `expression` | JSON: `result` |
| `GET /json/version`, `/json/list` | — | CDP discovery, `webSocketDebuggerUrl` rewritten through this service |
| `PUT /json/new` | — | create a target (Chrome requires PUT) |
| `GET /cdp/...` | — | CDP proxy, websocket upgrades pass through |

All POST routes take `-F` or `--data` form fields.

```shell
curl -s -H "authorization: Bearer $TOKEN" -X POST --data "url=https://example.com/" https://chromium.dev.initialed85.cc/render

curl -s -H "authorization: Bearer $TOKEN" -X POST --data "url=https://example.com/" https://chromium.dev.initialed85.cc/screenshot -o shot.png

curl -s -H "authorization: Bearer $TOKEN" -X POST --data "url=https://example.com/" \
  --data-urlencode "expression=document.title" https://chromium.dev.initialed85.cc/eval

# CDP discovery, then connect to the returned wss:// URL
curl -s -H "authorization: Bearer $TOKEN" https://chromium.dev.initialed85.cc/json/version
```

For a CDP client, take the `webSocketDebuggerUrl` from `/json/version` as-is — it
is rewritten to `wss://chromium.dev.initialed85.cc/cdp/devtools/browser/<id>`.

## Auth

The public host is bearer-token gated (2026-10-31): Traefik's `Middleware
auth-api-tokens@kubernetescrd` calls `deployment/token-gate` in namespace `auth` (source:
`~/Projects/Home/token-gate`), which answers 204 for a matching token and 401 for everything
else. **A 401 is the gate, not this service being down** — that distinction costs an hour of
diagnosis if the docs do not say it.

```shell
TOKEN=$(kubectl --context home-dev -n auth get secret api-tokens \
          -o jsonpath='{.data.API_TOKEN_EDWARD}' | base64 -d)
curl -s -H "authorization: Bearer $TOKEN" https://chromium.dev.initialed85.cc/healthz
```

The gate runs before this binary, so nothing here parses tokens, and the caller's
`authorization` header arrives in our request headers untouched (do not log it). Websocket
handshakes are gated the same way — measured on the real CDP endpoint: no credential → `401`,
`authorization: Bearer …` on the upgrade → `101`, `?token=` on the URL → `101`, wrong token →
`401`. websocat takes it as `--header="authorization: Bearer $TOKEN"` (equal-sign form, after
the address; websocat warns that a bare `-H` swallows following arguments). Local runs
(`BASE_URL=http://127.0.0.1:8080 ./validate.sh`) have no gate, no token.

## Things that bite

- Chrome rejects a CDP request whose `Host` header is not an IP or localhost, and
  rejects a websocket upgrade from a non-localhost origin. The proxy rewrites the
  Host and the browser runs with `--remote-allow-origins=*`, so connecting through
  the ingress works. Connecting straight to the loopback CDP port from outside the
  pod does not.
- The browser is allocated once on a long-lived context. Per-request tabs must be
  children of *that* context, and `tab()` derives them from `cfg.browserCtx` (the
  context that has run and so owns the browser). A chromedp context whose parent
  has no browser yet allocates its own headless shell, so parenting a tab on the
  request's own context launches a second browser per request: chromedp waits the
  browser, but its zygotes, gpu-process, utility and renderer are reparented to
  this binary (PID 1) and Go never `wait()`s them, so every request leaves about
  five `[defunct]` processes behind. Don't put a timeout on the context that
  first allocates the browser.
- An unreachable target looks like a success from the CDP side. Chrome serves its
  net-internals error page **under the target URL**, so a tab for that host exists
  in `/json/list` and `Runtime.evaluate` answers on it. Ask the page: the
  discriminator is `document.documentURI`, which is `chrome-error://chromewebdata/`
  (measured, for NXDOMAIN and for connection-refused). A caller that checks only
  "tab exists" and "evaluate answered" will file a bug against this service for its
  own dead DNS.
- Send CDP messages in one batch per websocket. Rapid sequential reconnects to the
  same page target get rejected and the message disappears without an error - with
  websocat that means piping several messages into one connection and matching
  replies by `id` (measured: nav, probe and evaluate on three sockets left the
  evaluate answering nothing; probe+evaluate on one socket was clean).
- Tabs are a budget: a navigation spends its tab, `/json/close` is not proxied, and
  each open websocket holds one of `MAX_CONCURRENT` slots, so a client that does not
  close what it opened (with `Target.closeTarget`, on the failure paths too) leaks
  tabs and slots. Count tabs before and after a run, sampling **before** you acquire
  a tab, and prove the scale with a keep-the-tab control run once.

## One browser, many tabs

Every request renders in its own tab of the one browser, so the tabs share that
browser's cookies, localStorage and HTTP cache: write from one `/eval` and the
next request reads it back. Live tabs are visible in `/json/list` while a request
is in flight, and they are closed when it finishes. Isolating requests needs
`chromedp.WithNewBrowserContext` per tab plus `target.DisposeBrowserContext` on
cancel; not worth it here.

Concurrency is capped at `MAX_CONCURRENT` (default 4); over that you get 429. Per
request timeout is `REQUEST_TIMEOUT_SECONDS` (default 30).

## Drawing: SwiftShader unless told otherwise

With `CHROME_EXTRA_FLAGS` unset the shell draws with ANGLE on SwiftShader and needs
nothing else. WebGL works, and in a *headless shell* it works without
`--enable-unsafe-swiftshader` (that deprecation gate is about real Chrome):
`WEBGL_debug_renderer_info` answers `ANGLE (Google, Vulkan 1.3.0 (SwiftShader Device
(Subzero) (0x0000C0DE)), SwiftShader driver)`.

`CHROME_EXTRA_FLAGS="--enable-gpu --use-angle=vulkan"` asks for the GPU. In the pod on hermes
(RTX 2080 Ti, driver 595.99.02), 8 screenshots of `webglsamples.org/aquarium` measured
**177–228 ms, mean 190 ms** with those flags against **362–527 ms, mean 427 ms** with
`CHROME_EXTRA_FLAGS` unset. Those are per-screenshot inside one warmed tab; at the API, where a
request also allocates a tab and navigates, the same page measured **0.78–0.98 s** on the GPU pod
against **1.88–3.41 s** on an identical pod with the flags empty — about 2.3x — while
`example.com` measured the same **0.14–0.25 s** either way. `nvidia-smi --query-compute-apps`
lists the pod's `headless-shell --type=gpu-process … --use-angle=vulkan` at 30 MiB, which is the
proof that the card is really in use rather than ANGLE merely claiming it was. `GET /` answers
`gl_renderer`, and the boot log says `drawing with …`, for the same reason.

Four things have to be true before ANGLE will take an NVIDIA card, all of them measured in
`hack/gpucheck` (see `hack/README-gpu.md`):

1. The container has to be handed the GPU — `nvidia.com/gpu` in `resources.limits` and
   `NVIDIA_DRIVER_CAPABILITIES: "all"`.
2. The image needs the X client libs (`libx11-6 libxext6 libxcb1 libx11-xcb1`, installed
   by the Dockerfile). Without them `libGLX_nvidia.so.0` dlopens but exports no Vulkan
   entry point, the loader logs `Could not get 'vkCreateInstance' via
   'vk_icdGetInstanceProcAddr' for ICD libGLX_nvidia.so.0`, and ANGLE is left holding
   llvmpipe.
3. There has to be a display to *open*. `DISPLAY=:99` with nothing behind it fails the way
   the previous item does, and it still fails once everything else below is in place, so the
   no-X shortcut Intel and AMD get does not apply here. Hence the `xvfb` sidecar in the
   home-ops deployment, sharing `/tmp/.X11-unix` through an emptyDir.
4. Something in the container has to advertise `VK_EXT_headless_surface` **to the Vulkan
   loader as an instance extension**, because that is what ANGLE asks for and it refuses to
   initialise without it. NVIDIA never advertises it: `vulkaninfo` lists it once, in the
   instance block, and never inside the NVIDIA device block. Pointing `VK_DRIVER_FILES` at
   `nvidia_icd.json` alone gives 24 instance extensions, none of them that one, and
   `--use-angle=vulkan` gives no GL context; add `/usr/share/vulkan/icd.d/lvp_icd.json`
   (Mesa's lavapipe, from `mesa-vulkan-drivers`, which the Dockerfile installs) and the same
   flags give `ANGLE (NVIDIA, Vulkan 1.4.329, RTX 2080 Ti)` — the extension comes from the
   software device and ANGLE then picks the discrete GPU as the device to draw with. Chrome's
   own bundled `vk_swiftshader_icd.json` advertises the extension and does not help; that
   pair still produces no context. So a software Vulkan device is load-bearing here even
   though every frame is drawn on the card, which is worth knowing before anyone trims the
   image.

Once a display exists `--use-angle=vulkan` alone is enough; `--enable-gpu` is along for
explicitness. Chrome keeps the **first** copy of a repeated switch — measured,
`--use-angle=swiftshader --use-angle=vulkan` ends on swiftshader — so extra flags add,
they never override what this binary already passes.

And if the backend that was asked for produces no GL context at all, `bootBrowser` says so
and starts a second browser with no extra flags rather than serve pages with dead WebGL:
a Vulkan backend with no display falls back to nothing, not even with
`--enable-unsafe-swiftshader` (measured). WebGPU is not in this shell either way —
`navigator.gpu` is missing with software and hardware GL alike.

## Pairs with ocr

Screenshot through chromium, read it back through `ocr.dev.initialed85.cc` — that's
the cheapest way to assert that a page actually rendered.

## Watch the processes

The pod has no `ps`. Count the defunct processes in it (`Z` in the third field of
`/proc/<pid>/stat`); steady state is 0. A steady climb means a browser is being
launched per request again.

```shell
POD=$(kubectl --context home-dev -n chromium get pods | tail -n +2 | awk '{print $1}')
kubectl --context home-dev -n chromium exec $POD -- sh -c '
  for p in /proc/[0-9]*; do set -- $(cat $p/stat 2>/dev/null); [ "$3" = Z ] && echo "$1"; done | wc -l'
```

The steady set is six live `headless-shell` processes under this binary (PID 1):
the browser, two zygotes, `--type=gpu-process`, one `--type=utility` service, and
one `--type=renderer` for the boot `about:blank` tab; a render adds a renderer of
its own while it runs.

## Validate

```shell
./validate.sh                                   # against the live service
BASE_URL=http://127.0.0.1:8080 ./validate.sh    # against a local container
```

## Notes

- The browser fetches arbitrary URLs from inside the cluster, so it can reach
  anything the cluster can reach. No allowlist — fine on the trusted network, worth
  restricting if this ever leaves it.
- Don't point `url` at the service's own ingress; a page that fetches its own host
  while the handler is busy can stall.
