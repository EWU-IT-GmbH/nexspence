# Pinned inputs: update deliberately and rerun the independent rebuild check.
FROM --platform=$BUILDPLATFORM golang:1.26.5@sha256:705e964a93a2fd2e75c7d59bb7d781b57e30f12293ffde5175c69229e18fb678 AS builder
WORKDIR /src
COPY . .
# Exported source bundles already contain vendor/ and can build without a network.
RUN if [ ! -f vendor/modules.txt ]; then go mod download && go mod vendor; fi
ARG VERSION=dev
ARG TARGETOS=linux
ARG TARGETARCH=amd64
ARG SOURCE_DATE_EPOCH
RUN CGO_ENABLED=0 GOOS=${TARGETOS} GOARCH=${TARGETARCH} go build \
    -mod=vendor -trimpath -buildvcs=false \
    -ldflags="-s -w -X main.Version=${VERSION}" -o /nexspence ./cmd/server

FROM --platform=$BUILDPLATFORM node:26-alpine@sha256:0b36e8c136b94cd4fcf02188228e76c31ad5872eef3fec8cbd2eee500cfd9e80 AS frontend-builder
WORKDIR /frontend
COPY frontend/ .
RUN if [ ! -d node_modules ]; then npm ci --no-audit --no-fund; fi
ARG SOURCE_DATE_EPOCH
RUN npm run build

FROM alpine:3.24@sha256:294b683cb724975bec92580e1e685676bd4b50bda910ddb8c51d4cabeaec77e6 AS runtime
# Avoid an unpinned apk transaction. CA roots and tzdata come from the pinned builder.
COPY --from=builder /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/ca-certificates.crt
COPY --from=builder /usr/share/zoneinfo/ /usr/share/zoneinfo/
ARG VERSION=dev
ARG REVISION
ARG SOURCE_DATE_EPOCH
WORKDIR /app
COPY --from=builder /nexspence /app/nexspence
COPY --from=builder /src/config.yaml.example /app/config.yaml
COPY --from=builder /src/deploy/docker-entrypoint.sh /app/entrypoint.sh
COPY --from=frontend-builder /frontend/dist /app/frontend/dist
COPY --from=builder /src/LICENSE /app/LICENSE
COPY --from=builder /src/NOTICE /app/NOTICE
COPY --from=builder /src/BUILDING-EWU.md /app/BUILDING-EWU.md
LABEL org.opencontainers.image.title="Nexspence EWU NuGet-search variant" \
      org.opencontainers.image.description="Modified Nexspence v2.5.1 with NuGet search" \
      org.opencontainers.image.licenses="AGPL-3.0-or-later" \
      org.opencontainers.image.source="https://github.com/EWU-IT-GmbH/nexspence" \
      org.opencontainers.image.url="https://github.com/EWU-IT-GmbH/nexspence" \
      org.opencontainers.image.documentation="https://github.com/EWU-IT-GmbH/nexspence" \
      org.opencontainers.image.revision="${REVISION}" \
      org.opencontainers.image.version="${VERSION}"
RUN addgroup -g 1000 nexspence && adduser -D -u 1000 -G nexspence nexspence \
    && mkdir -p /app/data/blobs /app/.cache /app/secrets \
    && chmod +x /app/entrypoint.sh && chown -R nexspence:nexspence /app \
    && epoch_days=$(( ${SOURCE_DATE_EPOCH:-0} / 86400 )) \
    && sed -i "s/^nexspence:!:.*$/nexspence:!:${epoch_days}:0:99999:7:::/" /etc/shadow
ENV HOME=/app
ENV TRIVY_CACHE_DIR=/app/.cache/trivy
USER 1000
EXPOSE 8081 5000
ENTRYPOINT ["/app/entrypoint.sh"]
CMD ["serve", "--config", "/app/config.yaml"]
