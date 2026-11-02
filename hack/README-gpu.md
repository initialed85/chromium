# Drawing on the GPU in this container

`hack/` is the harness behind the "Drawing" section of the README. It exists because every
statement in that section is something that had to be measured in the pod, and because the
failure modes all look like "no GPU" from the outside.

Everything below was measured on `home-dev`, node `hermes` (RTX 2080 Ti, driver 595.99.02,
docker 29.9.2 with the NVIDIA container toolkit and a CDI spec at `/var/run/cdi/nvidia.yaml`).

## The probe pod

`gpu-probe-pod.yaml` is a throwaway pod on hermes running the service image, with the GPU
handed to it. It is not part of the service; delete it when the experiment is over.

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
the page and `CHROME_REPEAT` runs a timed screenshot loop for the timing comparisons. The
answer that matters is `UNMASKED_RENDERER_WEBGL`, which names the ANGLE backend and the
device behind it.

Two traps in the probe itself, both of which produced a wrong answer before they were fixed:
a canvas can only ever have one context type, so asking one canvas for `webgl` and then for
`webgl2` reports `null context` for `webgl2` even when the GPU is right; and a `null` here
means "no context", not "software", so a broken GPU backend and a missing GPU look identical
until you also print the renderer string.

## What was measured

| flags | DISPLAY | WebGL renderer | aquarium screenshot |
|---|---|---|---|
| none | unset | `ANGLE (Google, Vulkan 1.3.0 (SwiftShader Device (Subzero)), SwiftShader driver)` | 451–677 ms |
| `--enable-gpu` | unset | **no context** | — |
| `--enable-gpu --use-angle=vulkan` | unset | **no context** | — |
| `--use-angle=vulkan` | `:99` (Xvfb) | `ANGLE (NVIDIA, Vulkan 1.4.329 (NVIDIA GeForce RTX 2080 Ti (0x00001E07)), NVIDIA)` | 193–218 ms |
| `--enable-gpu --use-angle=vulkan` | `:99` (Xvfb) | same | 193–218 ms |
| `--enable-gpu --use-angle=gl` | `:99` (Xvfb) | **no context** | — |
| `--enable-gpu --use-angle=vulkan` | `:0`, nothing behind it | **no context** | — |

Alongside those: `vulkaninfo --summary` in the container with the X client libs installed and
no `DISPLAY` ends with only `llvmpipe`, and the loader error

```
ERROR: loader_scanned_icd_add: Could not get 'vkCreateInstance' via
'vk_icdGetInstanceProcAddr' for ICD libGLX_nvidia.so.0
```

Set `DISPLAY` to a live Xvfb socket and the same command reports `NVIDIA GeForce RTX 2080 Ti`
at `apiVersion 1.4.329`. Disabling the NVIDIA implicit layer (`VK_LOADER_LAYERS_DISABLE`)
changes nothing, so the layer is not the culprit — the display connection is. On the host,
where there is no X server either, the ICD does enumerate headless; the difference is inside
the container, and the practical rule is simply: **give the container a display**.

VRAM while rendering is what settles "is the card actually being used". Sample
`nvidia-smi --query-gpu=utilization.gpu,memory.used` on hermes during a `CHROME_REPEAT=10`
run and the pod's footprint moves 3957 → 4134 MiB on the Vulkan runs and stays flat on the
SwiftShader ones. `nvidia-smi --query-compute-apps` never lists `headless-shell`, because a
Vulkan client is not a compute client.

## The recipe that worked

Three parts, and the image only covers two of them.

1. Hand the container the GPU: `nvidia.com/gpu: "1"` in `limits`,
   `NVIDIA_DRIVER_CAPABILITIES: "all"`.
2. Ship the X client libs, which the Dockerfile does: `libx11-6 libxext6 libxcb1 libx11-xcb1`.
   The image also installs `xvfb` so the same image can run the X server sidecar without a
   second image to build.
3. Run a display server in the pod and point `DISPLAY` at it. The deployment in home-ops
   `clusters/dev/applications/chromium/deployment.yaml` does it as a native sidecar plus an
   init container that waits for the socket, because the browser is allocated once at boot:
   a browser that starts before the display exists keeps a display-less GL for its whole
   life.

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
emptyDir — and `-nolisten tcp` keeps it off the network.

## Not chased further

- **WebGPU**: `navigator.gpu` is missing from this headless shell with software GL and with
  hardware GL alike, so Dawn is not in the build or is not enabled in headless. Untested
  whether a flag would bring it in.
- **`--use-angle=gl`** (ANGLE on native EGL/GLX): no context, with or without a display.
  NVIDIA's EGL does not offer a headless platform here and there is no reason to fight for it
  when Vulkan works.
- **AMD**: `edward-desktop` renders Vulkan fine (`radv`) and Mesa does implement
  `VK_EXT_headless_surface`, so an AMD node may need no X server at all. Not measured in a
  container, and `hermes` is where the `gpu-size: medium` label points.
