# PacketLab

<p align="center">
  <b>HTTP/HTTPS 流量捕获代理 — Capture. Inspect. Replay.</b>
</p>

<p align="center">
  <a href="LICENSE"><img src="https://img.shields.io/badge/license-MIT-blue.svg" alt="License"></a>
  <a href="https://go.dev"><img src="https://img.shields.io/badge/go-1.25+-00ADD8.svg" alt="Go"></a>
  <a href="https://github.com/user/packetlab/actions/workflows/ci.yml"><img src="https://github.com/user/packetlab/actions/workflows/ci.yml/badge.svg" alt="CI"></a>
</p>

---

PacketLab 是一个本地 HTTP/HTTPS 代理工具，用于捕获、检查、编辑和重放网络流量。面向安全研究人员、API 开发者和后端工程师。

## 功能

- **代理捕获** — HTTP 和 HTTPS 流量完整记录（方法、URL、请求/响应头、请求/响应体、耗时、大小）
- **HTTPS MITM** — 自签名 CA 证书，解密并明文查看 HTTPS 流量
- **拦截模式** — 自动放过 / 手动审批（Allow / Drop / Modify），手动模式下可编辑请求后转发
- **编辑重发** — 修改 Method / URL / Headers / Body 后通过代理重新发送
- **API 地图** — 按站点树形展示所有捕获的 API 端点，方法颜色区分，支持添加备注
- **实时推送** — WebSocket 将新请求即时同步到前端界面
- **批量写入** — WAL 模式 SQLite，50 条缓冲区 200ms 批量刷新，高流量不阻塞代理
- **搜索与过滤** — 按方法、URL、状态码、Host 搜索；支持错误筛选
- **i18n** — 中文 / English 界面切换
- **亮/暗主题** — CSS 变量双主题，localStorage 持久化

## 快速开始

### 安装

```bash
# 克隆仓库
git clone https://github.com/user/packetlab.git
cd packetlab

# 下载依赖
go mod download

# 编译
go build -o packetlab ./cmd/proxy/
```

### 运行

```bash
# 启动代理（端口 8080）+ Web 界面（端口 9090）
./packetlab

# 仅启动 API + 前端（不启动代理）
./packetlab --no-proxy

# 禁用 HTTPS 解密
./packetlab --no-mitm
```

访问 `http://localhost:9090`，配置浏览器 HTTP 代理为 `localhost:8080`。

> **安全说明（重要）**
> - API 默认启用鉴权：未设置 `--api-token` / `PACKETLAB_API_TOKEN` 时，启动自动生成随机 token 并写入 `~/.packetlab/token`（仅本用户可读）。
> - Web 界面首次访问会提示输入 token：`cat ~/.packetlab/token` 获取。
> - 状态变更 API 要求 `X-Requested-With: XMLHttpRequest` 头（前端自动携带），防止恶意网页跨站调用（CSRF）。
> - API 默认仅绑定 `127.0.0.1`。局域网访问需显式 `--api-host 0.0.0.0`，此时务必使用 `--api-token` 保护。

## HTTPS 证书安装

启动时自动生成自签名 CA 证书至 `~/.packetlab/certs/ca.crt`。

### macOS

```bash
sudo security add-trusted-cert -d -r trustRoot \
  -k /Library/Keychains/System.keychain \
  ~/.packetlab/certs/ca.crt
```

或双击 `ca.crt` → 钥匙串访问 → 系统 → 双击证书 → 信任 → 始终信任。

### Windows

```cmd
certutil -addstore Root %USERPROFILE%\.packetlab\certs\ca.crt
```

### Linux

```bash
sudo cp ~/.packetlab/certs/ca.crt /usr/local/share/ca-certificates/
sudo update-ca-certificates
```

## 命令行参数

| 参数 | 默认值 | 说明 |
|------|--------|------|
| `--proxy-port` | `8080` | 代理监听端口 |
| `--api-port` | `9090` | API/Web 服务端口 |
| `--db` | `~/.packetlab/data.db` | SQLite 数据库路径 |
| `--no-proxy` | `false` | 仅启动 API 服务，不启动代理 |
| `--no-mitm` | `false` | 禁用 HTTPS MITM 解密 |
| `--insecure` | `false` | 跳过上游 TLS 证书验证 |
| `--capture` | `false` | 启用网卡抓包（需 sudo / CAP_NET_RAW） |
| `--capture-iface` | `auto` | 指定抓包网卡 |
| `--capture-bpf` | `tcp` | BPF 过滤器 |
| `--capture-no-proc` | `false` | 禁用进程关联 |
| `--capture-stream-timeout` | `2` | 网卡抓包流空闲超时（分钟） |
| `--capture-ring-entries` | `262144` | 网卡抓包 ring buffer 条目数（向上取 2 的幂） |
| `--capture-max-streams` | `1000` | 最大并发 TCP 流数（超限 LRU 淘汰，最小 64） |
| `--intercept-pending-timeout` | `15s` | 待审请求超时（Go duration 格式，1s~10m） |
| `--cleanup-retention-days` | `7` | 自动清理保留天数 |
| `--cleanup-interval` | `6h` | 自动清理间隔（最小 1m） |
| `--api-allow-origins` | 空 | CORS/WebSocket 允许的 Origin（逗号分隔，默认仅 localhost） |
| `--max-req-body-kb` | `2048` | 请求体最大 KB |
| `--max-res-body-kb` | `4096` | 响应体最大 KB |

## 使用指南

### 基础抓包

1. 启动 PacketLab
2. 配置浏览器代理为 `localhost:8080`
3. 正常浏览网页 — 请求实时显示在左侧列表
4. 点击请求查看详情（请求头/体、响应头/体）

### 拦截模式

1. 点击顶部 **AUTO** 按钮切换为 **MANUAL**
2. 浏览器发起请求 → 待审请求出现在列表顶部（黄色左边框 + PENDING 徽章）
3. 点击待审请求 → 右侧显示预填的请求编辑器
4. 修改 Method/URL/Headers/Body
5. 点击 **Allow** 转发修改后的请求，或 **Drop** 丢弃

### API 地图

1. 切换到「API 地图」标签
2. 选择站点 → 树形展示所有捕获的 API 路径
3. 不同 HTTP 方法有不同颜色的左边框（GET 绿 / POST 蓝 / PUT 黄 / DELETE 红 ...）
4. 左键点击 → 按该路径过滤请求列表
5. 右键点击 → 添加备注 / 复制路径 / 按站点过滤

### 编辑重发

1. 点击任意已捕获请求
2. 切换到「重发」标签
3. 修改 Method / URL / Headers / Body
4. 点击「发送」

## API 端点

| 方法 | 路径 | 说明 |
|------|------|------|
| `GET` | `/api/requests` | 请求列表（支持 `method`, `search`, `host`, `error_only`, `limit`, `offset`） |
| `GET` | `/api/requests/:id` | 请求详情 |
| `DELETE` | `/api/requests/:id` | 删除请求 |
| `POST` | `/api/resend` | 通过代理重发请求 |
| `POST` | `/api/clear` | 清空所有记录 |
| `GET` | `/api/stats` | 统计信息（总数、错误数、数据量） |
| `GET`/`POST` | `/api/starred` | 收藏列表 / 切换收藏状态（`id`, `starred`） |
| `GET` | `/api/apimap` | API 地图树（参数 `host`） |
| `GET` | `/api/apimap/hosts` | 站点列表（参数 `search`, `limit`, `offset`） |
| `POST` | `/api/apimap/notes` | 添加/更新备注 |
| `DELETE` | `/api/apimap/notes/:id` | 删除备注 |
| `GET/POST` | `/api/intercept/mode` | 拦截模式查询/切换 |
| `GET` | `/api/intercept/pending` | 待审批请求列表 |
| `POST` | `/api/intercept/action` | 审批操作（allow/drop/modify） |
| `GET/POST` | `/api/intercept/rules` | 拦截规则（支持 `method` 维度） |
| `PUT/DELETE` | `/api/intercept/rules/:id` | 规则启用/删除 |
| `GET` | `/api/intercept/logs` | 拦截日志（`action`, `host`, `pattern`, `since`） |
| `GET/POST` | `/api/capture/status\|start\|stop` | 网卡抓包状态/启停 |
| `GET` | `/api/export/har` | 导出 HAR |
| `POST` | `/api/maintenance/cleanup` | 清理超期数据（`retention_days`） |
| `GET` | `/api/metrics` | 运行时指标（goroutine/内存/抓包统计） |
| `WS` | `/ws` | WebSocket 实时通知 |

## 架构

```
cmd/proxy/main.go          # 入口点
cmd/proxy/web/             # 内嵌 SPA 前端
  index.html               # HTML 骨架
  style.css                # 亮/暗双主题样式
  app.js                   # 前端逻辑
internal/
  proxy/                   # 代理核心
    proxy.go               # HTTP/HTTPS 代理 + MITM
    mitm.go                # CA 证书生成
    batch.go               # 批量写入器
    interceptor.go         # 拦截控制器
  api/                     # REST API
    server.go              # 路由与处理器
    middleware.go          # CORS / 限流
    ws.go                  # WebSocket Hub
  capture/                 # 网卡抓包引擎
    engine.go              # CaptureEngine + TCP Assembler
    engine_pcap.go         # libpcap 封装
    memring.go             # 内存环形缓冲 + 异步写入
    proc_unix.go           # 进程关联 (macOS/Linux)
    proc_windows.go        # 进程关联 (Windows)
  store/                   # SQLite 持久化
    store.go               # CRUD + API 地图 + 规则
  models/                  # 数据模型
    models.go              # CapturedRequest, InterceptRule, etc.
  config/                  # 配置管理
    config.go              # 环境变量 + 命令行参数
  llm/                     # LLM 流量解析（OpenAI/Anthropic/Gemini）
```

## 技术栈

| 组件 | 技术 |
|------|------|
| 语言 | Go 1.25+ |
| HTTP 代理 | elazarl/goproxy |
| HTTPS MITM | crypto/tls 自签名 CA |
| 数据库 | SQLite (modernc.org/sqlite, 纯 Go 无 CGO) |
| 实时通信 | WebSocket (gorilla/websocket) |
| 日志 | log/slog 结构化日志 |
| 前端 | Vanilla JS + CSS Variables（零依赖） |

## 快捷键

| 快捷键 | 功能 |
|--------|------|
| `Cmd/Ctrl + K` | 聚焦搜索框 |
| `Cmd/Ctrl + 1/2/3/4` | 切换请求/响应/重发/API地图 |
| `Cmd/Ctrl + Enter` | 发送重发请求 |
| `R` | 打开拦截规则面板 |
| 中键点击搜索框 | 清空搜索与过滤 |
| 右键 API 地图节点 | 上下文菜单 |

## 从源码构建

```bash
git clone https://github.com/user/packetlab.git
cd packetlab
go mod download
go build -o packetlab ./cmd/proxy/
```

## 贡献

请参阅 [CONTRIBUTING.md](CONTRIBUTING.md)。

## 更新日志

请参阅 [CHANGELOG.md](CHANGELOG.md)。

## 许可证

MIT License — 详见 [LICENSE](LICENSE) 文件。

## 免责声明

PacketLab 仅供合法的安全研究、API 调试和网络分析使用。请遵守当地法律法规，仅在您拥有权限的设备和网络上使用。使用者对自身行为承担全部责任。
