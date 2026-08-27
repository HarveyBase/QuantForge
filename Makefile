.PHONY: build web test vet clean run-backtest run

# 构建单二进制（含前端）
build: web
	rm -rf dashboard/webdist && cp -r web/dist dashboard/webdist
	go build -o bin/quantforge ./cmd/quantforge

# 前端构建
web:
	cd web && npm install && npm run build

# 测试与静态检查
test:
	go test ./...

vet:
	go vet ./...

# 本地跑（研究模式）
run: build
	./bin/quantforge serve -config config.json

# 命令行回测
run-backtest: build
	./bin/quantforge backtest -config config.json

clean:
	rm -rf bin web/dist dashboard/webdist
