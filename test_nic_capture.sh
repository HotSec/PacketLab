#!/bin/bash
# PacketLab 网卡抓包测试脚本
# 使用方法: sudo bash test_nic_capture.sh

set -e

BINARY="./packetlab"
API_PORT=9091
PROXY_PORT=8081

echo "============================================"
echo "  PacketLab 网卡抓包测试"
echo "============================================"

# 检查 root 权限
if [ "$EUID" -ne 0 ]; then
    echo "❌ 需要 root 权限运行: sudo bash $0"
    exit 1
fi
echo "✅ Root 权限确认"

# 检查二进制
if [ ! -f "$BINARY" ]; then
    echo "📦 构建二进制..."
    go build -o packetlab ./cmd/proxy/
fi
echo "✅ 二进制就绪: $BINARY"

# 清理旧进程
echo "🧹 清理旧进程..."
pkill -f "packetlab --api-port $API_PORT" 2>/dev/null || true
sleep 1

# 启动服务（带网卡抓包）
echo "🚀 启动 PacketLab (API=$API_PORT, Proxy=$PROXY_PORT, Capture=en0)..."
$BINARY --api-port $API_PORT --proxy-port $PROXY_PORT --capture &
PID=$!
sleep 3

# 检查服务健康
echo ""
echo "📡 检查服务状态..."
HEALTH=$(curl -s http://localhost:$API_PORT/health 2>/dev/null || echo "FAILED")
if [ "$HEALTH" = '{"status":"ok"}' ]; then
    echo "✅ API 服务正常"
else
    echo "❌ API 服务异常: $HEALTH"
    kill $PID 2>/dev/null
    exit 1
fi

# 检查抓包状态
CAPTURE_STATUS=$(curl -s http://localhost:$API_PORT/api/capture/status 2>/dev/null)
echo "📊 抓包状态: $CAPTURE_STATUS"

if echo "$CAPTURE_STATUS" | grep -q '"running":true'; then
    echo "✅ 网卡抓包引擎运行中"
else
    echo "❌ 网卡抓包引擎未运行"
    kill $PID 2>/dev/null
    exit 1
fi

# 记录当前请求数
BEFORE_TOTAL=$(curl -s http://localhost:$API_PORT/api/stats | python3 -c "import json,sys; print(json.load(sys.stdin).get('total',0))" 2>/dev/null || echo "0")
echo "📈 当前总请求数: $BEFORE_TOTAL"

# 生成 HTTP 流量（不经过代理，直连）
echo ""
echo "🌐 生成 HTTP 测试流量（直连，不走代理）..."
HTTP_RESP=$(curl -s http://httpbin.org/get 2>&1)
if echo "$HTTP_RESP" | grep -q '"url"'; then
    echo "✅ HTTP 请求成功"
else
    echo "⚠️  HTTP 请求可能失败"
fi

# 生成 HTTPS 流量（不经过代理，直连）
echo "🔒 生成 HTTPS 测试流量（直连，不走代理）..."
HTTPS_RESP=$(curl -s https://httpbin.org/get 2>&1)
if echo "$HTTPS_RESP" | grep -q '"url"'; then
    echo "✅ HTTPS 请求成功"
else
    echo "⚠️  HTTPS 请求可能失败"
fi

# 通过代理也生成一些流量
echo "🔄 通过代理生成流量..."
curl -s -x http://localhost:$PROXY_PORT http://httpbin.org/uuid > /dev/null 2>&1 || true
curl -s -x http://localhost:$PROXY_PORT https://httpbin.org/uuid > /dev/null 2>&1 || true
echo "✅ 代理流量已生成"

# 等待抓包引擎处理
echo ""
echo "⏳ 等待抓包引擎处理 TCP 流重组（约5秒）..."
sleep 5

# 检查新请求数
AFTER_TOTAL=$(curl -s http://localhost:$API_PORT/api/stats | python3 -c "import json,sys; print(json.load(sys.stdin).get('total',0))" 2>/dev/null || echo "0")
echo "📈 当前总请求数: $AFTER_TOTAL"

# 查询 NIC 模式的请求
echo ""
echo "📋 查询网卡抓包捕获的请求..."
NIC_COUNT=$(curl -s 'http://localhost:'$API_PORT'/api/requests?limit=50' | python3 -c "
import json, sys
data = json.load(sys.stdin)
nic_reqs = [r for r in data.get('data', []) if r.get('capture_mode') == 'nic']
proxy_reqs = [r for r in data.get('data', []) if r.get('capture_mode') == 'proxy']
print(f'NIC模式: {len(nic_reqs)} 条, Proxy模式: {len(proxy_reqs)} 条')
for r in nic_reqs[:5]:
    https = '🔒' if r.get('is_https') else '🌐'
    print(f'  {https} [{r[\"method\"]}] {r[\"url\"][:60]} → {r[\"status_code\"]}')
" 2>/dev/null || echo "解析失败")
echo "$NIC_COUNT"

# 最终统计
echo ""
echo "============================================"
echo "  测试结果"
echo "============================================"
NEW_TOTAL=$((AFTER_TOTAL - BEFORE_TOTAL))
echo "新增请求: $NEW_TOTAL 条"
echo "抓包状态: $CAPTURE_STATUS"

# 清理
echo ""
echo "🛑 停止服务..."
kill $PID 2>/dev/null || true
wait $PID 2>/dev/null || true
echo "✅ 测试完成"
