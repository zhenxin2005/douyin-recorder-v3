#!/bin/bash
# douyin-recorder-v3 服务器部署脚本（含 revoke 新增）
set -e
cd /opt/douyin-recorder-v3/server
echo "=== 1) 编译新版 ==="
/usr/local/go/bin/go build -o server.new .
ls -la server.new | awk '{print $5, $9}'
echo "=== 2) 备份旧版 ==="
TS=$(date +%Y%m%d%H%M%S)
cp server server.bak.$TS
echo "备份完成: server.bak.$TS"
echo "=== 3) 替换 + 重启 ==="
mv server.new server
systemctl restart douyin-recorder-v3
sleep 2
systemctl is-active douyin-recorder-v3
echo "=== 4) 健康检查 ==="
curl -s http://localhost:18080/health
echo ""
echo "=== 5) revoke 路由存在性（空码+错误密钥 → 401）==="
curl -s -o /dev/null -w "%{http_code}" -X POST http://localhost:18080/api/admin/revoke -H "Content-Type: application/json" -d '{"code":""}'
echo " (401=路由在线且鉴权生效)"
