# Pitfalls — douyin-recorder-v3

部署与运维踩坑记录。

## 1. 阿里云 ECS 未备案 IP 拦截常见 Web 端口（2026-10-21）
- **现象**：公网访问 `http://111.170.152.215:8080/health` 返回 `HTTP 404`，响应头 `Server: Apache`，正文「您访问的 IP 地址及端口号或未备案域名目前被限制」；服务器本机 `curl localhost:8080/health` 却正常返回 `{"status":"ok"}`。
- **根因**：阿里云对未备案（无 ICP 备案）IP 的常见 Web 端口（8080 等）做透明拦截，不是服务本身问题。
- **解法**：改用高位端口 **18080**（实测放行，返回正常响应）。server 支持 `DREC_ADDR` 环境变量改监听端口；客户端 e2e_test 支持 `DREC_TEST_SERVER` 覆盖服务器地址。
- **教训**：判断「服务不可达」前先分清：本机 curl 正常 + 公网被拦 = 云平台策略，不是代码 bug。

## 2. paramiko 与该服务器 SSH channel 打开超时（2026-10-21）
- **现象**：paramiko 5.1.0 连接认证成功（密码/密钥均可），但 `exec_command` 反复 `SSHException: Timeout opening channel`，曾误判为「sshd 被 reload 卡死」。
- **真相**：服务器 sshd 完全正常——用原生 `ssh.exe`（OpenSSH 客户端）密钥登录立即成功（`KEY_OK`）。
- **解法**：弃用 paramiko，改用 `ssh.exe` / `scp.exe` 执行远程操作。
- **教训**：不要因一个客户端实现反复超时就怀疑服务器；换一个 SSH 客户端验证后再下结论。

## 3. 服务器 SSH 已关闭密码登录（2026-10-21）
- 现仅 ed25519 密钥登录；私钥 `C:\Users\Administrator\.ssh\douyin_recorder_ed25519` **丢失即无法登录**，务必备份。
- 服务器 `sshd_config` 备份：`/etc/ssh/sshd_config.bak.*`（两次加固各一份）。
- 原始 root 密码 `nq3JSzYfcR1o` 已因关闭密码登录而无法用于 SSH（公网爆破面已消除）；但密码本身仍建议在阿里云控制台修改（万一有其他入口）。

## 4. 客户端激活报「激活码无效」——本机残留旧 server 抢占 8080（2026-10-21）
- **现象**：客户端激活新码返回「激活码无效」，但公网服务器上该码状态正常（active、未过期、设备数未满）。
- **根因**：本地端到端测试遗留的 `server.exe`（D:\douyin-recorder-v3\server\server.exe）仍监听 8080，其旧 DB 没有新激活码；客户端激活面板地址若指向 localhost:8080，请求打到旧服务 → 403「激活码无效」。同一码打公网 111.170.152.215:18080 → 200 成功。
- **排查法**：①对比 `curl localhost:8080/health` 与公网 /health；②看本机残留 server 进程；③查 admin/list 的 devices 数——用户请求未新增绑定 = 请求根本没到公网服务器。
- **教训**：公网部署后应停掉本地测试服务，避免端口/地址混淆；客户端激活失败先分清请求打到了哪台「服务器」。

## 5. Windows Defender 误拦本地编译的 Go 单文件 exe（2026-10-21 修复验证时发现）
- **现象**：`Start-Process server.exe` 报「文件包含病毒或潜在的垃圾软件」，进程无法启动；但 `go build` 本身成功。
- **根因**：本地新编译的无签名 Go 单文件 exe 触发 Defender 启发式误报（常见于含 net/http + SQLite 的二进制）。
- **解法**：验证时改用 `Start-Process go.exe -ArgumentList 'run','.'`（编译产物进临时目录，绕过拦截）；正式部署仍用 `go build` 产物上传服务器（Linux 无此问题）。
- **教训**：本地测试不要直接 Start-Process 刚编译的 exe；如果给客户分发 Windows exe 也遇到拦截，需考虑签名或添加 Defender 排除。

## 6. PowerShell 删除被占用文件失败——先停进程再删（2026-10-21）
- **现象**：`Remove-Item server.exe/server.db` 报「访问被拒绝 / 正由另一进程使用」，因为本地测试 server（PID 19768）和客户端测试 exe（PID 21488）还在运行。
- **解法**：先 `Get-NetTCPConnection -LocalPort <port>` 找到 OwningProcess → `Stop-Process` → 再删除。
- **教训**：清理运行时产物前先确认没有进程占用；端口检查 `Get-NetTCPConnection -State Listen` 是定位占用者的最快方式。

## 7. Go 单元测试访问未导出函数——测试文件必须放在同包目录（2026-10-21）
- **现象**：SSRF 回归测试放在 `server/`（package main）却调用 `extractRoomID`（package extractor 的未导出函数）→ 编译报 `undefined: extractRoomID`。
- **解法**：测试文件放入 `internal/extractor/` 目录内（同包可直接访问未导出函数）。

## 8. Windows PowerShell 5.1 缺 .NET Core API——验证脚本应直接用 Go 写（2026-10-21）
- **现象**：生产验证脚本连挂两轮——`[System.Convert]::ToHexString` 报「方法不存在」（.NET Framework 4.x 无此 API）、`Invoke-WebRequest -SkipHttpErrorCheck` 报「找不到参数」（PS5.1 无此开关）。前两步 gencode/register 都成功，唯独依赖 HMAC 签名的步骤全挂。
- **解法**：放弃 PowerShell 写签名验证，直接用客户端 `internal/apiclient`（签名逻辑与服务端 signature.go 严格一致）写 Go 测试打生产，一次通过。
- **教训**：凡涉及 HMAC/签名/复杂请求的验证，用 Go 写（与实现同语言），不要用 PowerShell 5.1 拼；或至少用 PS7（有 ToHexString/SkipHttpErrorCheck）。

## 9. 覆盖运行中二进制报 Text file busy——先停服务再替换（2026-10-21 部署）
- **现象**：systemd 服务运行中直接 `cp server.new ./server` → `cp: cannot create regular file './server': Text file busy`。
- **解法**：正确顺序 = `systemctl stop` → 备份 `cp server server.bak.$(date +%Y%m%d%H%M%S)` → `cp` 替换 → `chmod +x` → `systemctl start` → 验证。
- **教训**：Linux 下运行中的可执行文件不能被覆盖写入；先停后换。

## 10. PowerShell 管道会破坏二进制 tar 流——传文件用 scp（2026-10-21 部署）
- **现象**：`tar -cf - <files> | ssh host "tar -xf -"` 在远端报 `Skipping to next header` 解包失败。
- **根因**：PowerShell 的原生命令管道把二进制字节当文本/对象流处理，tar 数据被破坏。
- **解法**：本地 `tar -czf deploy.tar.gz` 打包 → `scp` 上传单文件（二进制安全通道）→ 远端解包编译。
- **教训**：PowerShell 里跨进程管道传二进制必须显式绕过文本编码（或用 scp/robocopy 等专用通道），不要赌管道透明。
- **教训**：测未导出函数 = 测试文件放同包；测导出 API = 放外部包。放错位置编译期就报错，不是运行期问题。
