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
- The browser is allocated once on a long-lived context. Per-request contexts are
  children of it, so a request timeout closes a tab, not the browser. Don't put a
  timeout on the context that first allocates the browser.

Concurrency is capped at `MAX_CONCURRENT` (default 4); over that you get 429. Per
request timeout is `REQUEST_TIMEOUT_SECONDS` (default 30).

## Pairs with ocr

Screenshot through chromium, read it back through `ocr.dev.initialed85.cc` — that's
the cheapest way to assert that a page actually rendered.

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
