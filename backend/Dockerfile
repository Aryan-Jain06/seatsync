# ---- build stage ----------------------------------------------------------
FROM golang:1.25-alpine AS build

WORKDIR /src

# Copy manifests first so dependency download is cached independently of
# source changes.
COPY go.mod go.sum ./
RUN go mod download

COPY . .

# A fully static binary, so the runtime image needs no libc and no shell.
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w" -o /out/server ./cmd/server \
    && CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w" -o /out/seed ./cmd/seed \
    && CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w" -o /out/migrate ./cmd/migrate

# ---- runtime stage --------------------------------------------------------
# scratch carries no package manager, shell or system libraries, which keeps
# the image tiny and leaves nothing for an attacker to pivot into. The cost is
# that everything the binary needs must be copied in explicitly.
FROM scratch

# Root certificates, needed for any outbound TLS.
COPY --from=build /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/ca-certificates.crt

COPY --from=build /out/server /server
COPY --from=build /out/seed /seed
COPY --from=build /out/migrate /migrate

# Numeric UID: scratch has no /etc/passwd for a name to resolve against.
USER 10001:10001

EXPOSE 8080

# The binary probes itself, since there is no curl in the image.
HEALTHCHECK --interval=10s --timeout=3s --start-period=5s --retries=5 \
    CMD ["/server", "-healthcheck"]

ENTRYPOINT ["/server"]
