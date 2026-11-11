# A Debian host booting systemd, with rootless Podman and Quadstack built from this checkout.
# make dev builds the web UI first; the binary embeds web/dist.
FROM golang:1.27 AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN test -f web/dist/index.html || { echo "web/dist is missing: run make web first" >&2; exit 1; }
RUN CGO_ENABLED=0 go build -ldflags="-X main.version=dev" -o /quadstack .

FROM debian:13
RUN apt-get update -qq && apt-get install -y -qq --no-install-recommends \
      systemd systemd-sysv dbus dbus-user-session podman uidmap passt acl ca-certificates >/dev/null \
    && rm -rf /var/lib/apt/lists/*
COPY --from=build /quadstack /usr/local/bin/quadstack
COPY deploy/quadstack.service /usr/local/share/quadstack/
COPY dev/host/ /
RUN systemctl enable dev-setup.service
STOPSIGNAL SIGRTMIN+3
CMD ["/sbin/init"]
