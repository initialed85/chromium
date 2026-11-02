# Headless Chromium behind a small HTTP/CDP API.
#
# The browser binary is the chromedp headless shell (~240 MB). Our binary launches
# it with a loopback CDP port and serves both the convenience endpoints and a CDP
# proxy (HTTP + websocket) on one port.
#
# No X server is needed to draw with SwiftShader, which is what happens when
# CHROME_EXTRA_FLAGS is empty. A hardware GPU does need one - see the apt layer below.
FROM golang:1.26-alpine AS build

WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download

COPY . .
RUN CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -o /chromium .

FROM chromedp/headless-shell:latest

# The X packages are for the hardware GPU path and they are load-bearing, not
# tidiness. Measured on hermes (RTX 2080 Ti, driver 595.99.02) inside this base image:
#
#   - libx11-6 libxext6 libxcb1 libx11-xcb1: libGLX_nvidia.so.0 - the NVIDIA Vulkan ICD
#     the container toolkit mounts - dlopens these before it will export any Vulkan entry
#     point. Without them the loader logs "Could not get 'vkCreateInstance' via
#     'vk_icdGetInstanceProcAddr' for ICD libGLX_nvidia.so.0", ANGLE is left holding only
#     software devices, and --use-angle=vulkan produces no GL context at all.
#   - xvfb: the ICD goes one further and only initialises when it can open a *display*.
#     A DISPLAY pointing at nothing fails the same way. So the X server has to exist,
#     and it ships here so the same image can run it as a sidecar container rather than
#     needing a second image - see hack/README-gpu.md and the chromium deployment under
#     home-ops clusters/dev/applications/chromium.
#   - mesa-vulkan-drivers: for its lavapipe ICD (/usr/share/vulkan/icd.d/lvp_icd.json),
#     which is the only Vulkan device here that advertises VK_EXT_headless_surface. ANGLE
#     asks the loader for that as an *instance* extension and refuses to initialise without
#     it, and NVIDIA's ICD never advertises it - vulkaninfo lists it once, in the instance
#     block, and never inside the NVIDIA device block. With only the NVIDIA ICD visible
#     (VK_DRIVER_FILES pointing at it alone) --use-angle=vulkan gives no GL context; with
#     lavapipe visible too the same flags give ANGLE (NVIDIA, Vulkan 1.4.329, RTX 2080 Ti),
#     because ANGLE then picks the discrete GPU as the device. Chrome's own bundled
#     SwiftShader ICD advertises the extension as well and does not help: that combination
#     still produces no context. So a software Vulkan device is load-bearing even though
#     every frame here is drawn on the card.
#   - nothing else from that package is used. Its intel/radeon/nouveau/virtio ICDs sit inert
#     on a node with no matching hardware, and llvmpipe is never selected while the card is.
#
# With no GPU asked for and CHROME_EXTRA_FLAGS unset, none of this is touched and the
# browser draws with SwiftShader exactly as it did before this layer existed.
RUN --mount=type=cache,target=/var/cache/apt --mount=type=cache,target=/var/lib/apt \
    apt-get update && apt-get install -y --no-install-recommends \
      xvfb libx11-6 libxext6 libxcb1 libx11-xcb1 mesa-vulkan-drivers

COPY --from=build /chromium /usr/local/bin/chromium

ENV PORT=8080
ENV CDP_PORT=9222
ENV CHROME_EXEC_PATH=/headless-shell/headless-shell
EXPOSE 8080
ENTRYPOINT ["/usr/local/bin/chromium"]
