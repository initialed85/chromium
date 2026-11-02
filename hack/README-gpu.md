# Drawing on the GPU in this container

`hack/` is the harness behind the "Drawing" section of the README. It exists because every
statement in that section is something that had to be measured in the pod, and because all of
the failure modes look identical from the outside: "no GL context".

Everything below was measured on `home-dev`, node `hermes` (RTX 2080 Ti, driver 595.99.02,
docker 29.9.2 with the NVIDIA container toolkit and a CDI spec at `/var/run/cdi/nvidia.yaml`),
in `chromedp/headless-shell` (Chrome 157, ANGLE on Vulkan, Vulkan loader 1.4.365 bundled at
`/headless-shell/libvulkan.so.1`).

## The probe pod

`gpu-probe-pod.yaml` is a throwaway pod on hermes running the service image, with the GPU
handed to it and an Xvfb sidecar. It is not part of the service; delete it when the experiment
is over.

```shell
kubectl --context home-dev apply -f hack/gpu-probe-pod.yaml
```

What the container was handed, from inside it:

```shell
kubectl --context home-dev -n chromium exec gpu-probe -- bash -c '
  ldconfig -p | grep -icE "GLX_nvidia|EGL_nvidia"   # toolkit mounted the GL/Vulkan driver
  ls /etc/vulkan/icd.d                              # nvidia_icd.json, mounted from the host
  ls /dev/dri /dev/nvidia*                          # card0, renderD128, nvidia0, nvidia-uvm
  nvidia-smi --query-gpu=name --format=csv,noheader'
```

With `nvidia.com/gpu` in `limits` and `NVIDIA_DRIVER_CAPABILITIES: "all"` that all answers.
Take the GPU limit away and none of it is there, which is the state the pod was in before
`Ask for nvidia GPU` (home-ops `3025fe3`).

## gpucheck

`gpucheck` is a tiny chromedp client that starts headless-shell with a flag set and prints
what the *page* sees. Judging a flag combo by which errors scroll past in a log is how this
investigation went wrong the first time.

```shell
CGO_ENABLED=0 go build -o /tmp/gpucheck ./hack/gpucheck
kubectl --context home-dev -n chromium cp /tmp/gpucheck gpu-probe:/gpucheck
kubectl --context home-dev -n chromium exec gpu-probe -- bash -c \
  'CHROME_FLAGS="--enable-gpu --use-angle=vulkan" /gpucheck'
```

`CHROME_FLAGS` is parsed the way the service parses `CHROME_EXTRA_FLAGS`. `CHROME_URL` picks
the page and `CHROME_REPEAT` runs a timed screenshot loop for the software/hardware
comparison. The answer that matters is `UNMASKED_RENDERER_WEBGL`, which names the ANGLE backend
and the device behind it.

Two traps in the probe itself, both of which produced a wrong answer before they were fixed:
a canvas can only ever have one context type, so asking one canvas for `webgl` and then for
`webgl2` reports `null context` for `webgl2` even when the GPU is right; and `null` here means
"no context", not "software", so a broken GPU backend and a missing GPU look identical until
you also print the renderer string.

chromedp swallows the browser's stderr. For the ANGLE and Vulkan-loader diagnostics below,
run the shell directly with `VK_LOADER_DEBUG=all` and read the stderr yourself.

## Which ICDs are visible, and why it decides everything

`vulkaninfo` prints an *instance* extension block before it prints any device. That block is
what ANGLE checks, and it is the union of whatever the visible ICDs advertise:

```shell
VK_DRIVER_FILES=/etc/vulkan/icd.d/nvidia_icd.json vulkaninfo | sed -n '1,40p'
#   Instance Extensions: count = 24        # no VK_EXT_headless_surface in there
VK_DRIVER_FILES=/usr/share/vulkan/icd.d/lvp_icd.json vulkaninfo | sed -n '1,40p'
#   Instance Extensions: count = 18        # VK_EXT_headless_surface present
```

and the same, from Chrome's side:

```shell
VK_DRIVER_FILES=/etc/vulkan/icd.d/nvidia_icd.json CHROME_FLAGS="--use-angle=vulkan" /gpucheck
#   "webgl": "null context"
VK_DRIVER_FILES=/usr/share/vulkan/icd.d/lvp_icd.json CHROME_FLAGS="--use-angle=vulkan" /gpucheck
#   "webgl": "null context"
# both visible:
#   "ANGLE (NVIDIA, Vulkan 1.4.329 (NVIDIA GeForce RTX 2080 Ti (0x00001E07)), NVIDIA)"
```

which is the whole story of this investigation. The error ANGLE produces when only the NVIDIA
ICD is visible is unambiguous but easy to skim past:

```
ERROR:ui/gl/angle_platform_impl.cc:34] vk_renderer.cpp:265 (VerifyExtensionsPresent):
  Extension not supported: VK_EXT_headless_surface
ERROR:ui/gl/angle_platform_impl.cc:34] Display.cpp:1506 (initialize): ANGLE Display::initialize
  error 0: Internal Vulkan error (-7): A requested extension is not supported,
  in ../../third_party/angle/src/libANGLE/renderer/vulkan/vk_renderer.cpp, enableInstanceExtensions:2477
ERROR:ui/gl/gl_display.cc:613] eglInitialize Vulkan failed with error EGL_NOT_INITIALIZED
```

NVIDIA never advertises that extension per device — on the host and in the container,
`grep headless_surface` over a full `vulkaninfo` hits exactly one line, in the instance block,
and the NVIDIA device block (lines 355–2162) does not contain it. Mesa's lavapipe does
advertise it, so installing `mesa-vulkan-drivers` makes the instance creation succeed, ANGLE
then ranks the discrete GPU above the CPU device, and every frame is drawn on the card. Neither
ICD works on its own: lavapipe alone gives Chrome no context either. Chrome's own bundled
`/headless-shell/vk_swiftshader_icd.json` advertises the extension too and gives no context in
combination with NVIDIA, measured with and without `--enable-unsafe-swiftshader`, so "the
extension is present at instance level" is necessary and not sufficient.

## What was measured

All rows in the service pod (`chromium-*` on hermes) or in `gpu-probe`, `DISPLAY=:99` from the
xvfb sidecar unless the row says otherwise.

| what was present | flags | WebGL renderer | aquarium screenshot |
|---|---|---|---|
| nothing (no GPU limit) | none | SwiftShader | — |
| NVIDIA ICD, no X client libs | none | SwiftShader | — |
| + X client libs, no display | `--enable-gpu --use-angle=vulkan` | **no context** | — |
| + display, no `mesa-vulkan-drivers` | `--enable-gpu --use-angle=vulkan` | **no context**, `VK_EXT_headless_surface` unsupported | — |
| + display, `VK_DRIVER_FILES=nvidia_icd.json` only | `--use-angle=vulkan` | **no context** | — |
| + display, `VK_DRIVER_FILES=lvp_icd.json` only | `--use-angle=vulkan` | **no context** | — |
| + display, `VK_DRIVER_FILES=nvidia+swiftshader` | `--use-angle=vulkan`, ±`--enable-unsafe-swiftshader` | **no context** | — |
| **display + NVIDIA ICD + lavapipe** | `--enable-gpu --use-angle=vulkan` | `ANGLE (NVIDIA, Vulkan 1.4.329 (NVIDIA GeForce RTX 2080 Ti (0x00001E07)), NVIDIA)` | 177–228 ms, mean **190 ms** |
| same | `--use-angle=vulkan` | same | same |
| same | `--use-angle=vulkan --ozone-platform=x11` | same | — |
| same | `--use-angle=gl`, `--use-gl=egl`, `--use-gl=desktop` | **no context** | — |
| display + NVIDIA ICD + lavapipe | none | `ANGLE (Google, Vulkan 1.3.0 (SwiftShader Device (Subzero)), SwiftShader driver)` | 362–527 ms, mean **427 ms** |

`--ozone-platform=x11` makes no difference, and neither does `--enable-gpu` once a display and
the extension exist: `--use-angle=vulkan` alone decides it. `nvidia-smi` is what settles whether
the card is really being used, and the clean read is the process table rather than the gauge:
with `--enable-gpu` in play `nvidia-smi --query-compute-apps=process_name,used_memory` lists
`/headless-shell/headless-shell --type=gpu-process … --use-angle=vulkan --enable-gpu` at 30 MiB,
and it lists nothing like that for a SwiftShader pod. Utilisation is harder to catch, because a
190 ms render is over long before a 2 s sample lands: six samples during 12 concurrent aquarium
screenshots peaked at 8%, and the pod's footprint on the card moved 3957 → 4134 MiB across a
`CHROME_REPEAT=8` run.

## At the API, which is the number that actually matters to a caller

The gpucheck loop reuses one warmed tab and takes screenshots in it, which is the right way to
compare backends and the wrong number to quote to a caller. A request allocates a tab and
navigates, so it is dominated by the page. Measured by running a second copy of the same image
(`1aa2641c0182`) on hermes with `CHROME_EXTRA_FLAGS=""` and no `nvidia.com/gpu` limit, and
alternating requests against both from a curl pod:

```yaml
# the software comparison pod; delete it and its Service when done
spec:
  nodeSelector: {kubernetes.io/arch: amd64, desktop-node: "true", gpu-size: "medium"}
  containers:
    - name: chromium
      image: docker.io/initialed85/chromium:<same sha as the deployment>
      env: [{name: CHROME_EXTRA_FLAGS, value: ""}]
```

| | GPU pod | same image, software |
|---|---|---|
| `POST /screenshot` aquarium, 5 alternating runs | 0.78, 0.80, 0.83, 0.88, 0.98 s | 1.88, 1.92, 2.00, 2.75, 3.41 s |
| `POST /screenshot` `example.com` | 0.14–0.25 s | 0.14–0.18 s |
| `POST /eval` `example.com` (`1+1`) | 0.088 s | 0.098 s |
| 12 parallel aquarium shots (`MAX_CONCURRENT=4`) | ~1.6 s each, nothing refused | |
| 6 parallel `example.com` shots | 4 at 200, 2 at 429 | |
| `GET /json/list` after the burst | 1 page tab | 1 page tab |

So: about 2.3x on a WebGL page, and nothing at all on a page with nothing to draw, which is the
expected shape for a GPU that only shortens the render half. The 429s are the concurrency
limiter doing its job, and no tab leak survived the burst.

## The recipe that worked

Four parts. The image covers three of them, the deployment covers the fourth.

1. Hand the container the GPU: `nvidia.com/gpu: "1"` in `limits`,
   `NVIDIA_DRIVER_CAPABILITIES: "all"`. Without "all" the toolkit mounts compute libraries
   only and there is no ICD to be found at all.
2. Ship the X client libs: `libx11-6 libxext6 libxcb1 libx11-xcb1`. `libGLX_nvidia.so.0`
   dlopens them before it will export a Vulkan entry point.
3. Ship `mesa-vulkan-drivers`, for lavapipe's `VK_EXT_headless_surface`. See the section
   above; nothing else in that package is used.
4. Run a display server in the pod and point `DISPLAY` at it — still required once 1–3 are in
   place (measured: `env -u DISPLAY` with everything else correct gives no context). The
   deployment in home-ops `clusters/dev/applications/chromium/deployment.yaml` does it as a
   native sidecar plus an init container that waits for the socket, because the browser is
   allocated once at boot: a browser that starts before the display exists keeps a
   display-less GL for its whole life.

```yaml
      initContainers:
        - name: xvfb                       # native sidecar: starts first, never exits
          restartPolicy: Always
          image: docker.io/initialed85/chromium:latest
          command: ["/usr/bin/Xvfb"]
          args: [":99", "-nolisten", "tcp", "-ac", "-screen", "0", "1280x1024x24"]
          volumeMounts: [{name: x11, mountPath: /tmp/.X11-unix}]
        - name: wait-for-x11               # only then does the browser start
          image: docker.io/initialed85/chromium:latest
          command: ["/bin/sh", "-c"]
          args: ["for i in $(seq 1 120); do [ -e /tmp/.X11-unix/X99 ] && exit 0; sleep 0.25; done; exit 1"]
          volumeMounts: [{name: x11, mountPath: /tmp/.X11-unix}]
      containers:
        - name: chromium
          env:
            - name: DISPLAY
              value: ":99"
            - name: CHROME_EXTRA_FLAGS
              value: "--enable-gpu --use-angle=vulkan"
            - name: NVIDIA_DRIVER_CAPABILITIES
              value: "all"
          volumeMounts: [{name: x11, mountPath: /tmp/.X11-unix}]
      volumes:
        - name: x11
          emptyDir: {}
```

`-ac` opens the socket to any local client — the socket only exists inside this pod's
emptyDir — and `-nolisten tcp` keeps it off the network. `xvfb` ships in the service image so
the sidecar does not need a second image to build.

## Not chased further

- **WebGPU**: `navigator.gpu` is missing from this headless shell with software GL and with
  hardware GL alike, so Dawn is not in the build or is not enabled in headless. Untested
  whether a flag would bring it in.
- **Native EGL/GLX** (`--use-angle=gl`, `--use-gl=egl`, `--use-gl=desktop`): no context, with
  or without a display. NVIDIA's EGL does not offer a headless platform here and there is no
  reason to fight for it when Vulkan works.
- **AMD**: `edward-desktop` renders Vulkan fine (`radv`) and Mesa implements
  `VK_EXT_headless_surface` in its drivers, so an AMD node may need neither a display nor a
  second ICD. Not measured in a container, and `gpu-size: medium` points at hermes.
- **A shorter ICD list than pulling all of mesa.** `VK_DRIVER_FILES` could name just
  `nvidia_icd.json` and `lvp_icd.json`, which is tidier than shipping intel/radeon/nouveau
  ICDs that sit inert. Not done: the flags the service passes are simpler to reason about than
  an env var whose absence silently changes the backend, and it has not been measured whether
  the loader honours `VK_DRIVER_FILES` the same way through Chrome's bundled loader.
