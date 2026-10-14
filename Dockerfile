# 诗词雅集 — 构建阶段
FROM golang:1.27-alpine AS build
WORKDIR /src
# 国内构建加速（可覆盖：--build-arg GOPROXY=direct）
ARG GOPROXY=https://goproxy.cn,direct
ENV GOTMPDIR=/tmp/gotmp
RUN mkdir -p /tmp/gotmp
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/server ./cmd/server

# 运行阶段（无 CGO，纯静态二进制，alpine 即可）
FROM alpine:3.20
WORKDIR /app
RUN adduser -D -u 1000 shici && mkdir -p /app/data && chown -R shici /app
USER shici
COPY --from=build /out/server /usr/local/bin/server
ENV PORT=8080 SHICI_DB=/app/data/shici.db
EXPOSE 8080
ENTRYPOINT ["/usr/local/bin/server"]
