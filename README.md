# W2A

OpenAI 兼容网关，单进程单端口（`:7863`），内置 Web 管理面板。

## 环境要求

- Docker + Docker Compose
- 若干账号凭证（通过 OAuth 登录获取）

## 部署

```bash
git clone https://github.com/sofs2005/w2a.git
cd w2a

# 面板登录口令（必填，未设置时 compose 直接报错退出）
echo 'WBGUI_PASSWORD=你的强口令' > .env

docker compose up -d
```

首次启动自动生成 `config/config.json`（含随机 `api_key`，权限 0600），key 打印在启动日志里：

```bash
docker compose logs w2a | grep api_key
```

## 添加账号

凭证落在 `auths/`，网关每 5 秒自动扫描入池，无需重启。

```bash
# 国内版
docker run --rm -it -v "$PWD/auths:/app/auths" --user root \
  --entrypoint /app/login ghcr.io/sofs2005/w2a:latest

# 国际版
docker run --rm -it -v "$PWD/auths:/app/auths" --user root \
  --entrypoint /app/login ghcr.io/sofs2005/w2a:latest --realm=global
```

重复执行即可连续添加。`--user root` 用于把产物 `chown` 给服务进程 uid（10001）。

宿主机若已有 Go / python3，也可用 `./login.sh`（多一步国际版注册地区完善与 trial 领取）。

非 root 宿主用户落盘后需移交属主：

```bash
chown -R 10001:10001 ./auths
```

之后新增账号建议直接用面板添加（同上属主天然一致）。

## 验证

```bash
curl -s http://localhost:7863/healthz
# {"healthy":2,"total":3,"service":"w2a"}

curl -s http://localhost:7863/v1/models -H "Authorization: Bearer your-api-key"

curl -sN http://localhost:7863/v1/chat/completions \
  -H "Authorization: Bearer your-api-key" \
  -H "Content-Type: application/json" \
  -d '{"model":"deepseek-v4-flash","messages":[{"role":"user","content":"hi"}],"stream":true}'
```

## 管理面板

浏览器打开 `http://<地址>:7863`。用户名 `admin`，口令为 `.env` 里的 `WBGUI_PASSWORD`（未设置时默认为 `w2a`，务必修改）。

| 变量 | 默认 | 说明 |
|---|---|---|
| `WBGUI_PASSWORD` | `w2a` | 面板登录口令 |
| `WBGUI_USERNAME` | `admin` | 面板登录用户名 |
| `WBGUI_SESSION_TTL` | `12h` | 会话有效期 |
| `WBGUI_READ_ONLY` | `false` | `true` = 全局只读 |
| `WBGUI_DANGEROUS_OPS` | `false` | `true` = 解锁删除账号等高危操作 |

面板配置只认环境变量，在 `docker-compose.yml` 的 `environment:` 段设置。

置于反向代理之后时，必须让代理透传原始主机名，否则写操作会因 CSRF 同源校验被拒：

```nginx
proxy_set_header Host              $http_host;
proxy_set_header X-Forwarded-Host  $http_host;
proxy_set_header X-Forwarded-Proto $scheme;
```

## 更新

```bash
docker compose pull && docker compose up -d
```

## Windows 原生运行

```powershell
go build -trimpath -ldflags="-s -w" -o wb2api.exe ./cmd/server
go build -trimpath -ldflags="-s -w" -o login.exe ./cmd/login

.\start-w2a.cmd
.\status-w2a.cmd
.\stop-w2a.cmd
```

PID 写入 `wb2api.pid`，日志写入 `data/server.out.log`、`data/server.err.log`。

## 源码构建

```bash
cd panel/web && npm ci && npm run build   # 前端产物到 panel/internal/webui/dist
cd ../..

CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o wb2api ./cmd/server
CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o login ./cmd/login
```

面板是独立 Go module，测试需分两处跑：

```bash
go test ./internal/... ./cmd/...
cd panel && go test ./...
```

## License

MIT，见 [LICENSE](LICENSE)。
