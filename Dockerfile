FROM node:22-alpine AS frontend-builder
WORKDIR /app/frontend
COPY frontend/package.json frontend/package-lock.json* ./
RUN npm install
COPY frontend/ ./
RUN npm run build

FROM golang:1.22-alpine AS backend-builder
ENV GOPROXY=https://goproxy.cn,direct
WORKDIR /app/backend
COPY backend/go.mod backend/go.sum* ./
RUN go mod download
COPY backend/ ./
RUN CGO_ENABLED=0 GOOS=linux go build -o /out/one-search ./cmd/server

FROM alpine:3.20 AS runtime-base
RUN apk add --no-cache ca-certificates curl nginx tzdata
WORKDIR /app
COPY --from=frontend-builder /app/frontend/dist /usr/share/nginx/html
COPY --from=backend-builder /out/one-search /usr/local/bin/one-search
COPY backend/migrations /app/backend/migrations
COPY deploy/nginx.conf /etc/nginx/http.d/default.conf
COPY deploy/all-in-one-entrypoint.sh /usr/local/bin/all-in-one-entrypoint.sh
RUN chmod +x /usr/local/bin/all-in-one-entrypoint.sh
EXPOSE 80

FROM runtime-base AS external
ENV ONE_SEARCH_EMBEDDED_POSTGRES=false \
    ONE_SEARCH_DEFAULT_DATABASE_MODE=external
ENTRYPOINT ["/usr/local/bin/all-in-one-entrypoint.sh"]

FROM runtime-base AS all-in-one
RUN apk add --no-cache postgresql postgresql-client su-exec
ENV ONE_SEARCH_EMBEDDED_POSTGRES=true \
    ONE_SEARCH_DEFAULT_DATABASE_MODE=embedded
VOLUME ["/var/lib/postgresql/data"]
ENTRYPOINT ["/usr/local/bin/all-in-one-entrypoint.sh"]
