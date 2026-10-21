# XUANJI.md — douyin-recorder-v3（版本3：SaaS 服务器授权版）

抖音直播录制工具 · 版本3 独立工程（与版本2 Wails 桌面版完全隔离，版本2代码保持不变）

## 项目定位

版本3 = **服务器授权版**：核心解析逻辑上云（服务器 API），客户端仿版本2界面但**解析走服务器**，**录制仍在本地 FLV 直录**（服务器带宽成本≈0）。用户需要激活码才能使用。

**架构决策**：版本3 是独立工程（`D:\douyin-recorder-v3\`），不复用/不修改版本2（`D:\douyin-recorder-wails\`）任何代码。

## 技术栈

| 维度 | 决策 |
|------|------|
| 服务器 | Go 1.27.3 + SQLite（modernc.org/sqlite 纯 Go，零 CGO，可部署 Ubuntu 22.04） |
| 客户端 | Wails v2 + Vite 7 + vanilla JS（仿版本2界面 + 激活面板） |
| 安全 | 激活码 + 设备绑定 + HMAC 签名防重放 + 限流（4 层防护） |
| 输出 | server.exe（服务器二进制）/ douyin-recorder-v3.exe（客户端 11.3MB） |

## 工程结构

```
D:\douyin-recorder-v3
├── server\                    # 服务器端（独立 go.mod，部署到 Linux）
│   ├── main.go                # 路由 + 鉴权中间件 + 解析接口（复用 extractor）
│   ├── store.go               # SQLite 存储（激活码/设备/token 只存 hash）
│   ├── auth.go                # 激活码注册/设备绑定/管理接口
│   ├── signature.go           # HMAC 签名校验 + 防重放
│   ├── ratelimit.go           # 限流（注册 10次/分·IP；解析 60次/分·设备）
│   └── internal\extractor\    # 流地址解析（从版本2复制）
├── client\                    # 版本3客户端（独立 go.mod，Wails 桌面）
│   ├── app.go                 # 绑定：Activate/GetActivationStatus/Parse/StartRecord/StopRecord/GetStatus/GetLastInfo
│   ├── main.go                # 窗口 980x720，标题「抖音直播录制工具 V3（服务器授权版）」
│   ├── internal\apiclient\    # 服务器 API 客户端（HMAC 签名/激活/token 本地持久化 + e2e 测试）
│   ├── internal\recorder\     # 本地 FLV 直录（从版本2复制）
│   └── frontend\              # 仿版本2 Dark Mode 界面 + 激活面板
└── README.md
```

## 服务器 API

| 方法 | 路径 | 鉴权 | 说明 |
|------|------|------|------|
| GET | /health | 无 | 健康检查 |
| POST | /api/register | 限流 | 激活码+设备ID → token（公开，注册时调用） |
| POST | /api/parse | X-Token + X-Timestamp + X-Nonce + X-Sign | 解析直播间，返回流地址/画质 |
| POST | /api/admin/gencode | X-Admin-Key（环境变量 DREC_ADMIN_KEY） | 生成激活码 |
| POST | /api/admin/list | X-Admin-Key | 查看激活码与绑定设备 |

**签名规则**（客户端 apiclient 与服务端 signature.go 严格一致）：
`msg = method\npath\ntimestamp\nnonce\nsha256hex(body)`；`sign = HMAC-SHA256(key=token, msg)`。

## 客户端激活流程

1. 启动 → `GetActivationStatus()` 检查本地配置（token 存在=已激活）
2. 未激活 → 显示激活面板（输入激活码 + 服务器地址，默认 http://localhost:8080）
3. `Activate(code, serverURL)` → POST /api/register → 保存 token 到 `v3-client-config.json`（exe 同目录）
4. 激活后 `Parse(url)` → 签名调服务器 /api/parse → 本地 recorder 直录

## 构建命令

```powershell
$env:PATH = "D:\dev\go\bin;C:\Users\Administrator\go\bin;C:\Program Files\nodejs;" + $env:PATH
# 服务器
cd D:\douyin-recorder-v3\server
go build -o server.exe .
# 客户端
cd D:\douyin-recorder-v3\client
wails build
# 产物：client\build\bin\douyin-recorder-v3.exe
```

## 验证进展（2026-10-21）

- [x] 服务器端迁移 + 独立 go.mod 编译通过
- [x] 客户端骨架复制 + apiclient 实现（HMAC 签名/token 持久化）
- [x] app.go 改造：Parse/StartRecord 走服务器 API，录制本地
- [x] 前端激活面板（Dark Mode 一致）
- [x] **本地端到端验证通过**（e2e_test.go）：激活成功 → 签名解析成功（房间 985706671742，13 画质，flv）→ 错误 token 被拒
- [x] wails build 打包成功（douyin-recorder-v3.exe 11.3MB）
- [x] **部署到真实服务器完成（111.170.152.215）**：装 Go 1.27.3 → 上传 server → 编译 17MB → systemd 守护（开机自启）→ SSH 加固（ed25519 密钥登录、密码登录已关）
- [x] **服务器端真实解析验证 PASS**（发码→注册→签名解析真实房间 985706671742，13 画质 flv）
- [x] **客户端连真实 IP 全链路验证 PASS**（health 200 / gencode 200 / register 200 / 签名解析通过鉴权 / 错误 token 401）
- [x] **代码审核 + 修复轮 PASS（2026-10-21）**：3 路并行审核发现 3 个 Critical + 重点 Important 全部修复并验证
  - Critical：SSRF（规范化抓取 URL + 响应限 10MB）· recorder 二次录制 panic（done 通道重建）· 凭证不进交付物（.gitignore）
  - Server：IP 级 parse 限流前移 · 先验签后登记 nonce + 容量上限 · body 1MB 限制 · http.Server 超时 · admin 接口 IP 限流 · device_id 字符集约束
  - Client：数据竞争 · 录制文件防覆盖（自动 _1/_2 后缀）· StartRecord 复用解析结果 · 配置损坏日志 · HLS ctx 化
  - Frontend：录制中断 UI 复位 · 激活码/URL 输入校验 · 恒 100% 进度条移除 · 日志上限/轮询 1000ms
  - 验证：server/client go vet + build 0 告警；SSRF/panic/防覆盖回归测试 PASS；本地 e2e PASS（激活→签名解析→错误 token 401）；wails build 打包成功
- [x] **修复版部署生产 + 全链路验证 PASS（2026-10-21 18:09）**：tar.gz 上传 → 服务器编译（17.3MB）→ 备份旧版 server.bak → systemd 重启 → 生产验证：health 200 / gencode 200 / register 200 / SSRF（evil.com 被拒「URL 格式不正确」）/ 合法 URL 鉴权通过 / 错误 token 401。旧二进制已备份 `server.bak.20261021101952`（回滚用）
- [ ] 付费模式（订阅制 vs 按次）尚未最终定
- [ ] 管理面板（发码界面）待定

## 部署环境备忘（2026-10-21）

| 项 | 值 |
|----|----|
| 服务器 | 111.170.152.215（阿里云 ECS，Ubuntu 22.04，4核 3.8G） |
| 服务 | systemd `douyin-recorder-v3`（/opt/douyin-recorder-v3/server/server） |
| 监听端口 | **:18080**（8080 被阿里云 ICP 备案拦截，18080 放行） |
| 管理密钥 | 环境变量 DREC_ADMIN_KEY（systemd unit 内），生成激活码需 X-Admin-Key 头 |
| SSH | **仅 ed25519 密钥登录**，密码登录已关闭；私钥 `C:\Users\Administrator\.ssh\douyin_recorder_ed25519`（务必保存，丢失即无法登录） |
| 客户端连接地址 | 前端默认值已改为 `http://111.170.152.215:18080`（index.html + main.js），开箱即用 |
| 端口支持 | server 支持 `DREC_ADDR` 环境变量改监听端口；e2e_test 支持 `DREC_TEST_SERVER` |
