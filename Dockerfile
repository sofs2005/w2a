# syntax=docker/dockerfile:1
# ── 阶段 1：面板前端 ────────────────────────────────────────────
# 面板（panel/）是 git subtree 引入的独立 module，仓库里不含其前端产物
# （dist/ 只有占位 index.html，真实 assets 被 gitignore），故必须构建。
FROM node:20-alpine AS web
WORKDIR /src
# 先只拷 manifest，让依赖层可缓存（面板源码改动不触发重新 npm ci）。
COPY panel/web/package.json panel/web/package-lock.json* ./panel/web/
RUN cd panel/web && (npm ci --no-audit --no-fund || npm install --no-audit --no-fund)
# 拷前端源码与 embed 占位目录（vite outDir 指向 ../internal/webui/dist）。
COPY panel/web ./panel/web
COPY panel/internal/webui/dist ./panel/internal/webui/dist
RUN cd panel/web && npm run build

# 版本必须与 go.mod 的 go 指令**保持一致**：CI 的 Go 版本由 go-version-file 从
# go.mod 推得，Dockerfile 这里是硬编码的第二个来源。两者一旦错开就会出现
# 「CI 校验用旧版、镜像编译用新版」——新版 API 能过构建却挂 CI（或反之）。
# 升级时两处一起改。
FROM golang:1.26-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
# replace 指向的本地 module 必须先存在，否则 go mod download 解析不到。
COPY panel/go.mod ./panel/
RUN go mod download
COPY . .
# 用前端阶段产出的真实 dist 覆盖源码树里的占位文件。
COPY --from=web /src/panel/internal/webui/dist ./panel/internal/webui/dist
# 一次编译全部二进制（工具进镜像，容器内可直接跑脚本）。全部 -trimpath -s -w。
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/wb2api ./cmd/server \
 && CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/signin_bin ./cmd/signin \
 && CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/login ./cmd/login \
 && CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/credit ./cmd/credit \
 && CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/trial_bin ./cmd/trial \
 && CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/activity_bin ./cmd/activity

# alpine:3.20 已于 2026-04-01 EOL，apk 源下线后构建会失败，保持使用仍在支持期的分支
FROM alpine:3.22
# python3：login.sh 的 JSON 解析 / 签到 / 落盘；bash：shell 脚本体。
RUN apk add --no-cache wget ca-certificates tzdata python3 bash \
 && adduser -D -u 10001 app \
 && mkdir -p /app/auths /app/data \
 && chown -R app:app /app
WORKDIR /app
# 脚本置入 + 去 CRLF（Windows 检出可能性）在切到 app 之前以 root 完成——
# app 对 root 所有文件无写权限，sed -i 需要写权限。
COPY --from=build /out/wb2api /app/wb2api
COPY --from=build /out/signin_bin /app/signin_bin
# 运维工具：宿主机无 Go 环境时用容器完成 OAuth 登录
#   docker run --rm -it -v "$PWD/auths:/app/auths" --user root \
#     --entrypoint /app/login ghcr.io/sofs2005/workbuddy2api:latest
COPY --from=build /out/login /app/login
COPY --from=build /out/credit /app/credit
COPY --from=build /out/trial_bin /app/trial_bin
COPY --from=build /out/activity_bin /app/activity_bin
COPY login.sh signin.sh credit.sh trial.sh /app/
# 国际版注册地区自动完善模块（login.sh global 分支 import；scripts/ 无测试/缓存）
COPY scripts/global_region.py /app/scripts/global_region.py
COPY scripts/task_common.py /app/scripts/task_common.py
COPY scripts/task_runner.py /app/scripts/task_runner.py
COPY scripts/school_open_day_2026.py /app/scripts/school_open_day_2026.py
RUN sed -i 's/\r$//' /app/*.sh && chmod 755 /app/*.sh
RUN sed -i 's/\r$//' /app/scripts/*.py && chmod 755 /app/scripts/*.py
# 镜像**不带**配置：首启由网关自动生成一份推荐配置（随机 api_key + 0600，见
# cmd/server/config.go WriteDefault）。刻意不 COPY config.example.json 当默认——
# 它带着 "test_key" 占位符，用户没挂载 config.json 时网关会静默跑在弱密钥上，
# 而 listen 绑的是 0.0.0.0。示例文件仍留在仓库里供对照，只是不进镜像。
USER app
EXPOSE 7863
HEALTHCHECK --interval=30s --timeout=5s --start-period=5s \
  CMD wget -qO- http://127.0.0.1:7863/healthz || exit 1
ENTRYPOINT ["/app/wb2api", "-config", "/app/config/config.json"]
