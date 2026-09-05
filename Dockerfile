FROM node:22.23.2-alpine3.23 AS frontend-builder
WORKDIR /app/frontend
COPY frontend/package.json frontend/package-lock.json ./
RUN npm ci
COPY frontend/ ./
RUN npm run build

FROM golang:1.26.8-alpine3.23 AS backend-builder
ENV GOPROXY=https://goproxy.cn,direct
WORKDIR /app/backend
COPY backend/go.mod backend/go.sum* ./
RUN go mod download
COPY backend/ ./
RUN CGO_ENABLED=0 GOOS=linux go build -o /out/searchmeld ./cmd/server

FROM alpine:3.22.5 AS runtime-base
# Refresh installed APKs too; a supported base tag can predate security fixes.
RUN apk upgrade --no-cache && \
    apk add --no-cache ca-certificates nginx tzdata \
      'libcrypto3>=3.5.8-r0' 'libssl3>=3.5.8-r0'
ENV TZ=Asia/Shanghai
WORKDIR /app
COPY --from=frontend-builder /app/frontend/dist /usr/share/nginx/html
COPY --from=backend-builder /out/searchmeld /usr/local/bin/searchmeld
COPY backend/migrations /app/backend/migrations
COPY deploy/nginx.conf /etc/nginx/http.d/default.conf
COPY deploy/all-in-one-entrypoint.sh /usr/local/bin/all-in-one-entrypoint.sh
RUN chmod +x /usr/local/bin/all-in-one-entrypoint.sh && \
    ln -s /usr/local/bin/searchmeld /usr/local/bin/one-search
EXPOSE 80

FROM runtime-base AS external
ENV SEARCHMELD_EMBEDDED_POSTGRES=false \
    SEARCHMELD_DEFAULT_DATABASE_MODE=external
ENTRYPOINT ["/usr/local/bin/all-in-one-entrypoint.sh"]

FROM runtime-base AS all-in-one
# Keep the baseline PGDATA major; changing it requires an operator-led migration.
RUN apk add --no-cache postgresql16 postgresql16-client su-exec
ENV SEARCHMELD_EMBEDDED_POSTGRES=true \
    SEARCHMELD_DEFAULT_DATABASE_MODE=embedded
VOLUME ["/var/lib/postgresql/data"]
ENTRYPOINT ["/usr/local/bin/all-in-one-entrypoint.sh"]
