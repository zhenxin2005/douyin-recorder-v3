# ============================================================
# douyin-recorder-v3 激活码管理工具（方案 B：本地脚本）
# 功能：生成激活码 / 查看列表 / 禁用激活码
# 用法：双击 drec-admin.bat，或直接运行本脚本
# 管理密钥：读取同目录 admin-key.txt（首次运行自动引导创建）
# ============================================================
# 服务器地址（内置；如需更换服务器，修改这一行即可）
$ServerURL = 'http://111.170.152.215:18080'

# ---- 管理密钥读取 ----
$KeyFile = Join-Path $PSScriptRoot 'admin-key.txt'
$AdminKey = ''
if (Test-Path $KeyFile) {
    $AdminKey = (Get-Content $KeyFile -Raw).Trim()
}
if ([string]::IsNullOrEmpty($AdminKey)) {
    Write-Host ''
    Write-Host '首次使用：请把服务器管理密钥填入下面的文件，然后重新运行本脚本。' -ForegroundColor Yellow
    Write-Host "文件位置：$KeyFile" -ForegroundColor Yellow
    Write-Host ''
    $input = Read-Host '或者现在直接输入管理密钥（不会保存到文件）'
    if ([string]::IsNullOrEmpty($input)) {
        Write-Host '未提供密钥，退出。' -ForegroundColor Red
        exit 1
    }
    $AdminKey = $input.Trim()
} else {
    # 密钥文件存在则直接使用，同时校验有效性
    Write-Host ("使用管理密钥：{0}..." -f $AdminKey.Substring(0, [Math]::Min(4, $AdminKey.Length)))
}

$Headers = @{ 'X-Admin-Key' = $AdminKey }

# ---- 请求封装 ----
function Invoke-AdminApi {
    param(
        [string]$Path,
        [hashtable]$Body
    )
    try {
        $params = @{
            Uri         = "$ServerURL$Path"
            Method      = 'Post'
            Headers     = $Headers
            ContentType = 'application/json'
        }
        if ($Body) {
            $params.Body = ($Body | ConvertTo-Json)
        }
        $resp = Invoke-RestMethod @params
        return $resp
    } catch {
        $status = $_.Exception.Response.StatusCode.value__
        $detail = $_.ErrorDetails.Message
        Write-Host ("请求失败 (HTTP {0}): {1}" -f $status, $detail) -ForegroundColor Red
        return $null
    }
}

# ---- 功能：生成激活码 ----
function New-Codes {
    Write-Host ''
    Write-Host '=== 生成激活码 ===' -ForegroundColor Cyan
    $daysStr = Read-Host '有效期天数（0 = 永久）'
    $days = 0
    if ($daysStr -match '^\d+$') { $days = [int]$daysStr }
    $note = Read-Host '备注（如买家姓名/渠道，可留空）'
    $nStr = Read-Host '生成数量（1-100，默认 1）'
    $n = 1
    if ($nStr -match '^\d+$' -and [int]$nStr -ge 1) { $n = [int]$nStr }
    if ($n -gt 100) { $n = 100 }

    $resp = Invoke-AdminApi '/api/admin/gencode' @{ days = $days; note = $note; n = $n }
    if ($resp -and $resp.codes) {
        Write-Host ''
        Write-Host ("已生成 {0} 个激活码：" -f $resp.codes.Count) -ForegroundColor Green
        $i = 1
        foreach ($c in $resp.codes) {
            Write-Host ("  {0}. {1}" -f $i, $c) -ForegroundColor White
            $i++
        }
        if ($note) { Write-Host ("备注：{0}" -f $note) -ForegroundColor DarkGray }
        Write-Host ("有效期：{0}" -f $(if ($days -gt 0) { "$days 天" } else { '永久' })) -ForegroundColor DarkGray
        Write-Host ''
        Write-Host '⚠ 请立即复制保存，本工具不会重复显示已生成的激活码。' -ForegroundColor Yellow
    }
}

# ---- 功能：查看激活码列表 ----
function Show-Codes {
    Write-Host ''
    Write-Host '=== 激活码列表 ===' -ForegroundColor Cyan
    $resp = Invoke-AdminApi '/api/admin/list' $null
    if ($null -eq $resp) { return }
    $codes = $resp.codes
    if ($codes.Count -eq 0) {
        Write-Host '（暂无激活码）' -ForegroundColor DarkGray
        return
    }
    Write-Host ''
    Write-Host ('{0,-22} {1,-8} {2,-20} {3,-6} {4}' -f '激活码', '状态', '过期时间', '设备数', '备注') -ForegroundColor Cyan
    Write-Host ('{0,-22} {1,-8} {2,-20} {3,-6} {4}' -f ('-'*22), ('-'*8), ('-'*20), ('-'*6), ('-'*10)) -ForegroundColor DarkGray
    foreach ($c in $codes) {
        $statusText = switch ($c.status) {
            'unused'  { '未使用' }
            'active'  { '使用中' }
            'revoked' { '已禁用' }
            default   { $c.status }
        }
        $expText = if ($c.expires_at -gt 0) {
            [DateTimeOffset]::FromUnixTimeSeconds($c.expires_at).ToLocalTime().ToString('yyyy-MM-dd HH:mm')
        } else { '永久' }
        $statusColor = switch ($c.status) {
            'unused'  { 'Green' }
            'active'  { 'Yellow' }
            'revoked' { 'Red' }
            default   { 'Gray' }
        }
        Write-Host ('{0,-22} ' -f $c.code) -NoNewline
        Write-Host ('{0,-8} ' -f $statusText) -ForegroundColor $statusColor -NoNewline
        Write-Host ('{0,-20} {1,-6} {2}' -f $expText, $c.devices, $c.note)
    }
    Write-Host ''
    Write-Host ("共 {0} 个激活码" -f $codes.Count) -ForegroundColor DarkGray
}

# ---- 功能：禁用激活码 ----
function Revoke-Code {
    Write-Host ''
    Write-Host '=== 禁用激活码 ===' -ForegroundColor Cyan
    $code = Read-Host '输入要禁用的激活码（如 DREC-XXXX-XXXX-XXXX）'
    $code = $code.Trim().ToUpper()
    if ($code -eq '') {
        Write-Host '未输入，取消。' -ForegroundColor DarkGray
        return
    }
    Write-Host ("确认禁用 {0}？禁用后已绑定的设备将立即失效。" -f $code) -ForegroundColor Yellow
    $confirm = Read-Host '输入 y 确认'
    if ($confirm.Trim().ToLower() -ne 'y') {
        Write-Host '已取消。' -ForegroundColor DarkGray
        return
    }
    $resp = Invoke-AdminApi '/api/admin/revoke' @{ code = $code }
    if ($resp -and $resp.revoked) {
        Write-Host ("✅ 已禁用：{0}" -f $resp.revoked) -ForegroundColor Green
    }
}

# ---- 主菜单 ----
function Show-Menu {
    Write-Host ''
    Write-Host '========================================' -ForegroundColor DarkGray
    Write-Host ' douyin-recorder-v3 激活码管理' -ForegroundColor White
    Write-Host (" 服务器：{0}" -f $ServerURL) -ForegroundColor DarkGray
    Write-Host '========================================' -ForegroundColor DarkGray
    Write-Host ' 1. 生成激活码'
    Write-Host ' 2. 查看激活码列表'
    Write-Host ' 3. 禁用激活码'
    Write-Host ' 0. 退出'
    Write-Host ''
}

# ---- 主循环 ----
$running = $true
while ($running) {
    Show-Menu
    $choice = Read-Host '请选择'
    switch ($choice.Trim()) {
        '1' { New-Codes }
        '2' { Show-Codes }
        '3' { Revoke-Code }
        '0' { $running = $false }
        default { Write-Host '无效选择，请重试。' -ForegroundColor Red }
    }
}
Write-Host '已退出。' -ForegroundColor DarkGray
