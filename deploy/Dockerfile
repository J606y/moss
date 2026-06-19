# syntax=docker/dockerfile:1
# Moss server 镜像（多阶段，自包含单二进制：内嵌前端）
# 构建：在仓库根执行  docker build -f deploy/Dockerfile -t moss .

# ---- 阶段1：构建前端 ----
FROM node:20-bookworm-slim AS web
WORKDIR /src/web
COPY web/package.json web/package-lock.json ./
RUN npm ci
COPY web/ ./
RUN npm run build

# ---- 阶段2：构建 server（CGO_ENABLED=0 + embed_web 内嵌前端）----
FROM golang:1.26-bookworm AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
# 注入前端产物供 //go:embed all:dist 使用
COPY --from=web /src/web/dist ./server/dist
# 预备一个空数据目录，供最终镜像 chown 给 nonroot
RUN mkdir -p /app-data
ARG VERSION=dev
RUN CGO_ENABLED=0 GOOS=linux GOARCH=amd64 \
    go build -tags embed_web -trimpath -ldflags "-s -w" \
    -o /out/moss-server ./server

# ---- 阶段3：极小运行镜像（distroless 静态，nonroot）----
FROM gcr.io/distroless/static-debian12:nonroot
WORKDIR /app
COPY --from=build /out/moss-server /app/moss-server
# /app/data 归属 nonroot(65532)，使 nonroot 可写；命名卷会继承该属主
COPY --from=build --chown=65532:65532 /app-data /app/data
VOLUME ["/app/data"]
EXPOSE 8787
USER 65532:65532
ENTRYPOINT ["/app/moss-server"]
CMD ["--listen", ":8787", "--data", "/app/data"]
