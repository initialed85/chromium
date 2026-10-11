# Headless Chromium behind a small HTTP/CDP API.
#
# The browser binary is the chromedp headless shell (~240 MB, no X server
# needed). Our binary launches it with a loopback CDP port and serves both the
# convenience endpoints and a CDP proxy (HTTP + websocket) on one port.
FROM golang:1.26-alpine AS build

WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download

COPY . .
RUN CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -o /chromium .

FROM chromedp/headless-shell:latest

COPY --from=build /chromium /usr/local/bin/chromium

ENV PORT=8080
ENV CDP_PORT=9222
ENV CHROME_EXEC_PATH=/headless-shell/headless-shell
EXPOSE 8080
ENTRYPOINT ["/usr/local/bin/chromium"]
