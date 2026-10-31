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
curl -s -X POST --data "url=https://example.com/" https://chromium.dev.initialed85.cc/render

curl -s -X POST --data "url=https://example.com/" https://chromium.dev.initialed85.cc/screenshot -o shot.png

curl -s -X POST --data "url=https://example.com/" \
  --data-urlencode "expression=document.title" https://chromium.dev.initialed85.cc/eval

# CDP discovery, then connect to the returned wss:// URL
curl -s https://chromium.dev.initialed85.cc/json/version
```

For a CDP client, take the `webSocketDebuggerUrl` from `/json/version` as-is — it
is rewritten to `wss://chromium.dev.initialed85.cc/cdp/devtools/browser/<id>`.

## Two things that bite

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

## One browser, many tabs

Every request renders in its own tab of the one browser, so the tabs share that
browser's cookies, localStorage and HTTP cache: write from one `/eval` and the
next request reads it back. Live tabs are visible in `/json/list` while a request
is in flight, and they are closed when it finishes. Isolating requests needs
`chromedp.WithNewBrowserContext` per tab plus `target.DisposeBrowserContext` on
cancel; not worth it here.

Concurrency is capped at `MAX_CONCURRENT` (default 4); over that you get 429. Per
request timeout is `REQUEST_TIMEOUT_SECONDS` (default 30).

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
