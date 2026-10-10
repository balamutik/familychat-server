FROM node:26-alpine AS admin-build
WORKDIR /src/admin
COPY admin/package*.json ./
RUN npm ci --no-audit --no-fund
COPY admin ./
RUN npm run build

FROM golang:1.27-bookworm AS go-build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY cmd ./cmd
COPY internal ./internal
RUN CGO_ENABLED=0 go build -trimpath -ldflags='-s -w' -o /out/familychat ./cmd/familychat

FROM debian:trixie-slim
# Current libheif understands auxiliary image references in recent iPhone HEICs.
# The HEVC decoder is a separate package, omitted by --no-install-recommends.
RUN apt-get update && apt-get install -y --no-install-recommends ca-certificates ffmpeg libheif-examples libheif-plugin-libde265 librsvg2-bin && rm -rf /var/lib/apt/lists/* && useradd --uid 10001 --create-home familychat
COPY --from=go-build /out/familychat /usr/local/bin/familychat
COPY --from=admin-build /src/admin/dist /app/admin
ENV ADMIN_STATIC_DIR=/app/admin
RUN mkdir -p /var/lib/familychat/crypto && chown -R 10001:10001 /var/lib/familychat && chmod 700 /var/lib/familychat/crypto
USER 10001:10001
EXPOSE 8080
ENTRYPOINT ["/usr/local/bin/familychat"]
CMD ["serve"]
