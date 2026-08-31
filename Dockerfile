# 构建阶段：前端产物已随仓库（dashboard/webdist），Go 编译即得带 UI 的二进制
FROM golang:1.25-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -o /quantforge ./cmd/quantforge

# 运行阶段：单二进制，配置与数据卷挂载
FROM alpine:3.20
RUN apk add --no-cache ca-certificates tzdata
COPY --from=build /quantforge /usr/local/bin/quantforge
VOLUME ["/data"]
EXPOSE 8080
ENTRYPOINT ["quantforge"]
CMD ["serve", "-config", "/data/config.json"]
