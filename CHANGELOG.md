# Changelog

## v0.0.1 (2026-05-22) — First Tagged Release

> PacketLab 首个正式版本 — MITM 代理、API 地图、拦截编辑重发，即开即用。

### Highlights

| 模块 | 核心能力 |
|------|----------|
| **代理捕获** | HTTP/HTTPS 全流量记录（Method、URL、Headers、Body、耗时、大小、状态码） |
| **HTTPS MITM** | 自签名 CA 证书解密，一键安装引导，可配置 MITM 排除列表 |
| **拦截模式** | 自动放过 / 手动审批（Allow / Drop / Modify），待审批黄色标记 |
| **编辑重发** | 修改 Method/URL/Headers/Body 后通过代理重新发送 |
| **API 地图** | 按站点树形展示全部端点，方法颜色区分，状态码标记，添加备注，右键菜单 |
| **实时推送** | WebSocket 将新请求即时同步到前端界面 |
| **批量写入** | WAL 模式 SQLite，50 条缓冲区 200ms 批量刷新 |
| **搜索过滤** | 按方法/URL/状态码/Host 搜索；错误筛选；搜索式 host 下拉 |
| **i18n** | 中文 / English 界面切换 |
| **明暗主题** | CSS 变量双主题，localStorage 持久化，平滑过渡 |
| **交互细节** | 交错入场动画、Tab 滑动指示器、可拖拽分隔面板 |

### Commits (20)

```
a3d1b66 docs: 网卡抓包详细实施方案
ae7eeaf docs: 开源项目文档
991ea68 feat: API地图点击用host+路径搜索 + 列表动画柔和化
ab26c74 fix: API地图多层展开时子路径被父级遮挡
e38c855 fix: 7项审查修复 — 去重/日志/初始化/过滤/错误处理
44b5d60 fix: 拦截修改后放行不生效
a8604b2 fix: 请求记录不写入SQLite — 捕获代码在拦截器分支后永不执行
e7d66dd fix: 修复 SQLite 打开时 DSN 参数导致磁盘 I/O 错误
44ee409 feat: 不再记录 CONNECT 隧道请求
b2940a9 fix: 最终优化 — 4项修复
2bbafce fix: 端到端审查 — 修复3个问题
60c702e fix: API地图显示网站全部URL — host带端口导致数据分裂
661837d fix: API地图显示 / 节点 + 根节点自身方法
952aa49 fix: API地图根节点 leaf 不显示 — renderTreeNode 只遍历children
20d84b2 fix: API地图支持显示 / 路径的 CONNECT 请求
78aeb8c fix: API地图树形结构children数据不全 — 两个关键bug
96844a9 fix: API地图 host 带端口(:443)时无数据
deadbfb fix: 修复API地图树形视图不显示
45db165 fix: 修复API地图数据不显示 + 移除加载更多站点
fd41aa8 fix: 修复点击请求记录无响应 - ID类型不匹配
```

### Quick Start

```bash
# 克隆 & 构建
git clone https://github.com/user/packetlab.git
cd packetlab
go mod download
go build -o packetlab ./cmd/proxy
./packetlab

# 代理默认端口 :8080，Web UI 默认 :8081
# 安装 CA 证书: ~/.packetlab/certs/ca.crt
```

