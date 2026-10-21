#!/bin/bash
# 生产服务器 revoke 全链路验证：发码→注册→禁用→parse 403→再注册被拒
set -e
cd /opt/douyin-recorder-v3/server
echo "=== 0) 部署状态核对 ==="
systemctl is-active douyin-recorder-v3
ls -la server.bak.* 2>/dev/null | tail -1 || echo "WARN: 无 server.bak 备份文件"
echo ""
echo "=== 1) 读取管理密钥 ==="
if [ -f .admin_key ]; then
  ADMIN_KEY=$(cat .admin_key)
elif [ -f /etc/systemd/system/douyin-recorder-v3.service ] && grep -q DREC_ADMIN_KEY /etc/systemd/system/douyin-recorder-v3.service; then
  ADMIN_KEY=$(grep -oP 'DREC_ADMIN_KEY=\K[^" ]+' /etc/systemd/system/douyin-recorder-v3.service)
else
  echo "FATAL: 找不到管理密钥"; exit 1
fi
echo "密钥长度: ${#ADMIN_KEY}（不显示内容）"
echo ""
echo "=== 2) 运行 revoke e2e 测试（打生产 18080）==="
DREC_TEST_SERVER="http://localhost:18080" DREC_TEST_ADMIN_KEY="$ADMIN_KEY" /usr/local/go/bin/go test ./ -run TestRevokeE2E -v -timeout 120s
echo ""
echo "=== 3) 完成 ==="
