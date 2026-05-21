# PacketLab

HTTP/HTTPS 流量捕获代理工具。支持实时捕获、历史记录、编辑重发、API 地图树形视图。

## 功能

- **HTTP/HTTPS 代理捕获** — 完整记录请求头、请求体、响应头、响应体、耗时、大小
- **HTTPS MITM 解密** — 自签名 CA 证书，解密并查看 HTTPS 流量明文
- **历史记录** — SQLite 持久化，按方法/URL/状态码过滤，分页查询
- **编辑重发** — 修改 Method/URL/Headers/Body 后重新发送并记录
- **API 地图** — 按站点树形展示接口路径，方法颜色区分，支持添加备注
- **实时推送** — WebSocket 新请求即时同步到前端
- **i18n** — 中英文界面切换
- **亮/暗主题** — CSS 变量双主题，localStorage 持久化
- **大流量优化** — 批量事务写入，请求体 32KB / 响应体 64KB 截断，WAL 模式

## 快速开始

```bash
# 编译
cd traffic-capture-tool
go build -o packetlab ./cmd/proxy/

# 启动（代理 8080 + Web 界面 9090）
./packetlab

# 仅启动 API + 前端（不启动代理）
./packetlab --no-proxy

# 禁用 HTTPS MITM
./packetlab --no-mitm
```

访问 `http://localhost:9090`，配置浏览器代理为 `localhost:8080`。

## 命令行参数

| 参数 | 默认值 | 说明 |
|------|--------|------|
| `--proxy-port` | `8080` | 代理监听端口 |
| `--api-port` | `9090` | API/Web 服务端口 |
| `--db` | `~/.packetlab/data.db` | SQLite 数据库路径 |
| `--no-proxy` | `false` | 仅启动 API，不启动代理 |
| `--no-mitm` | `false` | 禁用 HTTPS MITM 解密 |

## HTTPS MITM 证书安装

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

## API 端点

| 方法 | 路径 | 说明 |
|------|------|------|
| `GET` | `/api/requests?method=&search=&host=&error_only=&limit=&offset=` | 请求列表 |
| `GET` | `/api/requests/:id` | 请求详情 |
| `DELETE` | `/api/requests/:id` | 删除请求 |
| `POST` | `/api/resend` | 重发请求 |
| `POST` | `/api/clear` | 清空历史 |
| `GET` | `/api/stats` | 统计信息 |
| `GET` | `/api/apimap?host=` | API 地图树 |
| `GET` | `/api/apimap/hosts?search=&limit=&offset=` | 站点列表 |
| `POST` | `/api/apimap/notes` | 添加/更新备注 |
| `DELETE` | `/api/apimap/notes/:id` | 删除备注 |
| `WS` | `/ws` | WebSocket 实时推送 |

## 架构

```
cmd/proxy/main.go          # 入口，embed 前端
internal/
  proxy/                   # 代理核心
    proxy.go               # HTTP/HTTPS 代理，goproxy MITM
    mitm.go                # CA 证书生成
    batch.go               # 批量写入器
  api/                     # REST API + WebSocket
    server.go              # 路由与处理器
    ws.go                  # WebSocket Hub
  store/                   # SQLite 持久化
    store.go               # CRUD + API地图查询
  models/                  # 数据模型
    models.go
cmd/proxy/web/index.html   # 前端 SPA（内嵌）
```

## 技术栈

- **Go** — 代理服务器 + API + WebSocket
- **SQLite** (modernc.org/sqlite) — 纯 Go 无 CGO
- **goproxy** (elazarl/goproxy) — HTTP/HTTPS MITM 代理
- **gorilla/websocket** — WebSocket 实时推送
- **CSS Variables** — 亮/暗双主题

## 快捷键

| 快捷键 | 功能 |
|--------|------|
| `Cmd/Ctrl + K` | 聚焦搜索框 |
| `Cmd/Ctrl + 1/2/3/4` | 切换请求/响应/重发/API地图 |
| `Cmd/Ctrl + Enter` | 发送重发请求 |
| `中键点击搜索框` | 清空搜索与过滤 |
| `右键 API 地图节点` | 上下文菜单 |

## License

MIT
