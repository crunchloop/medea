# Build the medea binary and ship it on distroless.
# Build from the medea/ module root:
#   docker build -f deploy/Dockerfile -t medea:dev .
#
# syntax=docker/dockerfile:1

FROM golang:1.26 AS build
WORKDIR /src
# Cache deps first (the Talos main-module import pulls a large tree).
COPY go.mod go.sum ./
RUN go mod download
COPY . .
# CGO_ENABLED=1 is REQUIRED: the 1Password Go SDK refuses to compile without cgo
# on linux/darwin (its !cgo file is a hard compile error), and its desktop-app
# path uses import "C" — so the binary links libc. Hence distroless/base (glibc),
# not distroless/static. golang:1.26 ships gcc.
RUN CGO_ENABLED=1 go build -trimpath -ldflags="-s -w" -o /out/medea ./cmd/medea

FROM gcr.io/distroless/base-debian12:nonroot
COPY --from=build /out/medea /usr/local/bin/medea
USER nonroot:nonroot
EXPOSE 7600
ENTRYPOINT ["medea"]
# Override with your own flags; mount state at /var/lib/medea and config at /etc/medea.
CMD ["serve", "--listen", "0.0.0.0:7600", \
     "--store", "/var/lib/medea/medea.db", \
     "--creds-dir", "/var/lib/medea/creds", \
     "--snapshot-dir", "/var/lib/medea/snapshots", \
     "--token-file", "/etc/medea/token", \
     "--tls-cert", "/etc/medea/tls/cert.pem", \
     "--tls-key", "/etc/medea/tls/key.pem", \
     "--rollouts"]
