#!/usr/bin/env bash
# Validate the Chromium service end to end.
#
#   ./validate.sh                        # against https://chromium.dev.initialed85.cc
#   BASE_URL=http://127.0.0.1:8080 ./validate.sh
#
# If an OCR service is reachable (OCR_URL, or the default ocr.dev host), the
# screenshot is read back through it, which validates both services at once.
set -euo pipefail

BASE_URL="${BASE_URL:-https://chromium.dev.initialed85.cc}"
OCR_URL="${OCR_URL:-https://ocr.dev.initialed85.cc}"
TARGET="${TARGET:-https://example.com/}"

work="$(mktemp -d)"
trap 'rm -rf "$work"' EXIT

echo "==> $BASE_URL/healthz"
curl -fsS "$BASE_URL/healthz" | grep -q '"ok":true' && echo "healthz ok"

echo "==> POST $BASE_URL/render"
render="$(curl -fsS -X POST --data "url=$TARGET" "$BASE_URL/render")"
printf '%s\n' "$render"
printf '%s' "$render" | grep -q '"status":200' || { echo "FAIL: expected status 200" >&2; exit 1; }
printf '%s' "$render" | grep -q 'Example Domain' || { echo "FAIL: expected the page title in the render" >&2; exit 1; }

echo "==> POST $BASE_URL/eval"
evalres="$(curl -fsS -X POST --data "url=$TARGET" --data-urlencode "expression=1+1" "$BASE_URL/eval")"
printf '%s\n' "$evalres"
printf '%s' "$evalres" | grep -q '"result":2' || { echo "FAIL: eval did not return 2" >&2; exit 1; }

echo "==> POST $BASE_URL/screenshot"
curl -fsS -X POST --data "url=$TARGET" "$BASE_URL/screenshot" -o "$work/shot.png"
file "$work/shot.png" | grep -q "PNG image data" || { echo "FAIL: screenshot is not a PNG" >&2; exit 1; }
echo "screenshot ok"

echo "==> POST $BASE_URL/json/version (rewritten websocket url)"
ws="$(curl -fsS "$BASE_URL/json/version" | grep -o 'wss\?://[^\"]*')"
printf '%s\n' "$ws"
printf '%s' "$ws" | grep -q "/cdp/devtools/browser/" || { echo "FAIL: webSocketDebuggerUrl was not rewritten" >&2; exit 1; }

echo "==> PUT $BASE_URL/json/new (Chrome requires PUT, not GET)"
new="$(curl -fsS -X PUT "$BASE_URL/json/new")"
printf '%s\n' "$new"
printf '%s' "$new" | grep -q '"webSocketDebuggerUrl":"wss\?://' || { echo "FAIL: /json/new did not return a rewritten target" >&2; exit 1; }
target_id="$(printf '%s' "$new" | grep -o '"id":"[^"]*"' | cut -d'"' -f4)"

if command -v node >/dev/null 2>&1; then
    echo "==> CDP websocket upgrade through $BASE_URL/cdp/"
    node -e '
      const events = [];
      const pending = new Map();
      let nextId = 0;
      const send = (method, params = {}, sessionId) => new Promise((resolve, reject) => {
        const id = ++nextId;
        pending.set(id, resolve);
        ws.send(JSON.stringify({ id, sessionId, method, params }));
        setTimeout(() => reject(new Error(method + " timed out")), 20000);
      });

      const ws = new WebSocket(process.argv[1]);
      ws.onmessage = (e) => {
        const m = JSON.parse(String(e.data));
        if (m.id) { pending.get(m.id)(m); pending.delete(m.id); }
        else events.push(m.method);
      };
      ws.onerror = (e) => { console.error("ws error", e.message); process.exit(1); };

      ws.onopen = async () => {
        const closed = await send("Target.closeTarget", { targetId: process.argv[2] });
        if (closed.error) { console.error("closeTarget failed:", JSON.stringify(closed.error)); process.exit(1); }
        console.log("target closed:", closed.result.success);

        const target = await send("Target.createTarget", { url: "about:blank" });
        const targetId = target.result.targetId;
        const session = (await send("Target.attachToTarget", { targetId, flatten: true })).result.sessionId;

        await send("Runtime.enable", {}, session);
        await send("Runtime.evaluate", { expression: "console.log(\"from the page\")" }, session);
        for (let i = 0; i < 20 && !events.includes("Runtime.consoleAPICalled"); i++) {
          await new Promise((r) => setTimeout(r, 500));
        }
        if (!events.includes("Runtime.consoleAPICalled")) {
          console.error("no event through the proxy, got:", events.join(", ") || "nothing");
          process.exit(1);
        }
        console.log("event through the proxy:", events.join(", "));

        await send("Target.closeTarget", { targetId });
        ws.close();
      };
    ' "$ws" "$target_id"
fi

if curl -fsS "$OCR_URL/healthz" >/dev/null 2>&1; then
    echo "==> OCR the screenshot through $OCR_URL"
    text="$(curl -fsS -F "file=@$work/shot.png" -F "psm=6" "$OCR_URL/ocr")"
    printf '%s\n' "$text"
    printf '%s' "$text" | grep -qi "example" || { echo "FAIL: OCR did not read the page" >&2; exit 1; }
fi

echo "all checks passed"
