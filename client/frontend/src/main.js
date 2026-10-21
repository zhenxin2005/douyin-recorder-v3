import './style.css';
import './app.css';
import { Parse, StartRecord, StopRecord, GetStatus, GetLastInfo, GetActivationStatus, Activate } from '../wailsjs/go/main/App';

// ---- DOM 引用 ----
const urlInput = document.getElementById('url');
const parseBtn = document.getElementById('parseBtn');
const infoCard = document.getElementById('infoCard');
const avatar = document.getElementById('avatar');
const anchorName = document.getElementById('anchorName');
const liveStatus = document.getElementById('liveStatus');
const roomId = document.getElementById('roomId');
const curQuality = document.getElementById('curQuality');
const recTime = document.getElementById('recTime');
const qualityList = document.getElementById('qualityList');
const recBtn = document.getElementById('recBtn');
const barIn = document.getElementById('barIn');
const metaLeft = document.getElementById('metaLeft');
const metaRight = document.getElementById('metaRight');
const logBox = document.getElementById('log');

// 激活面板
const activateWrap = document.getElementById('activateWrap');
const actCode = document.getElementById('actCode');
const actServer = document.getElementById('actServer');
const actBtn = document.getElementById('actBtn');
const actMsg = document.getElementById('actMsg');

// ---- 状态 ----
let currentUrl = '';
let qualityKey = '';
let recording = false;
let pollTimer = null;
let lastInfo = null;

// ---- 工具函数 ----
function log(text, cls = '') {
    const div = document.createElement('div');
    if (cls) div.className = cls;
    div.textContent = text;
    logBox.appendChild(div);
    // 日志上限 200 条，防止 DOM 无限累积
    while (logBox.children.length > 200) logBox.removeChild(logBox.firstChild);
    logBox.scrollTop = logBox.scrollHeight;
}

function fmtTime(sec) {
    sec = Math.max(0, Math.floor(sec));
    const h = Math.floor(sec / 3600);
    const m = Math.floor((sec % 3600) / 60);
    const s = sec % 60;
    const pad = (n) => String(n).padStart(2, '0');
    return h > 0 ? `${pad(h)}:${pad(m)}:${pad(s)}` : `${pad(m)}:${pad(s)}`;
}

function fmtSize(bytes) {
    if (bytes < 1024 * 1024) return (bytes / 1024).toFixed(1) + ' KB';
    if (bytes < 1024 * 1024 * 1024) return (bytes / (1024 * 1024)).toFixed(1) + ' MB';
    return (bytes / (1024 * 1024 * 1024)).toFixed(2) + ' GB';
}

// ---- 解析 ----
async function handleParse() {
    const url = urlInput.value.trim();
    if (!url) {
        log('请先粘贴直播间链接', 'err');
        return;
    }
    // 本地校验：URL 必须指向抖音直播
    if (!/live\.douyin\.com/.test(url)) {
        log('✗ URL 不是抖音直播链接（应形如 https://live.douyin.com/1234567890）', 'err');
        return;
    }
    parseBtn.disabled = true;
    parseBtn.textContent = '解析中…';
    try {
        const info = await Parse(url);
        lastInfo = info;
        currentUrl = url;

        // 主播信息
        avatar.textContent = (Array.from(info.anchor_name || '?')[0]);
        anchorName.textContent = info.anchor_name || '未知主播';
        roomId.textContent = info.room_id || '—';

        if (info.is_live) {
            liveStatus.innerHTML = '<span class="live-dot"></span>正在直播';
        } else {
            liveStatus.innerHTML = '<span class="live-dot off"></span>未开播 / 直播已结束';
        }

        // 画质选项
        qualityList.innerHTML = '';
        const opts = info.quality_options || [];
        if (opts.length === 0) {
            const item = document.createElement('div');
            item.className = 'q-item';
            item.textContent = '默认画质';
            qualityList.appendChild(item);
        } else {
            opts.forEach((q, i) => {
                const item = document.createElement('div');
                item.className = 'q-item' + (i === 0 ? ' on' : '');
                item.textContent = q.label;
                item.dataset.key = q.quality_key;
                item.addEventListener('click', () => {
                    qualityList.querySelectorAll('.q-item').forEach((x) => x.classList.remove('on'));
                    item.classList.add('on');
                    qualityKey = q.quality_key;
                    curQuality.textContent = q.label;
                });
                qualityList.appendChild(item);
            });
        }
        qualityKey = opts.length > 0 ? opts[0].quality_key : '';
        curQuality.textContent = opts.length > 0 ? opts[0].label : '默认画质';

        infoCard.classList.remove('hidden');
        recBtn.disabled = false;
        log(`✓ 解析成功：房间 ${info.room_id || '?'} · ${info.anchor_name || '未知'}`, 'ok');
        log(`  格式 ${info.stream_format || 'flv'} · 画质选项 ${opts.length} 个`);
    } catch (err) {
        infoCard.classList.add('hidden');
        recBtn.disabled = true;
        log(`✗ 解析失败：${String(err && err.message ? err.message : err)}`, 'err');
    } finally {
        parseBtn.disabled = false;
        parseBtn.textContent = '解析';
    }
}

// ---- 录制 ----
async function toggleRecord() {
    if (!recording) {
        // 开始
        if (!currentUrl) {
            log('请先解析直播间', 'err');
            return;
        }
        recBtn.disabled = true;
        try {
            const output = await StartRecord(currentUrl, qualityKey);
            log(`● ${new Date().toLocaleTimeString()} 开始录制 → ${output}`, 'live');
            metaLeft.textContent = `写入 ${output}`;
            recording = true;
            recBtn.classList.add('recording');
            recBtn.innerHTML = '停<br>止';
            startPolling();
        } catch (err) {
            log(`✗ 录制启动失败：${String(err && err.message ? err.message : err)}`, 'err');
        } finally {
            recBtn.disabled = false;
        }
    } else {
        // 停止
        try {
            await StopRecord();
            log(`■ ${new Date().toLocaleTimeString()} 已停止录制`, 'ok');
            stopPolling();
            recording = false;
            recBtn.classList.remove('recording');
            recBtn.innerHTML = '开<br>始';
        } catch (err) {
            log(`✗ 停止失败：${String(err && err.message ? err.message : err)}`, 'err');
        }
    }
}

function startPolling() {
    stopPolling();
    pollTimer = setInterval(async () => {
        try {
            const st = await GetStatus();
            if (!st.is_recording) {
                // 后端已停止但界面还在录制态（异常中断/直播断开）→ 复位 UI
                if (recording) {
                    stopPolling();
                    recording = false;
                    recBtn.classList.remove('recording');
                    recBtn.innerHTML = '开<br>始';
                    recTime.textContent = '00:00';
                    log('■ 录制已中断（直播断开或异常结束）', 'err');
                }
                return;
            }
            recTime.textContent = fmtTime(st.duration);
            metaRight.textContent = `${fmtSize(st.file_size)} · ${(st.speed || 0).toFixed(1)} MB/s`;
        } catch (e) {
            // 轮询失败静默
        }
    }, 1000);
}

function stopPolling() {
    if (pollTimer) {
        clearInterval(pollTimer);
        pollTimer = null;
    }
}

// ---- 事件绑定 ----
parseBtn.addEventListener('click', handleParse);
recBtn.addEventListener('click', toggleRecord);
urlInput.addEventListener('keydown', (e) => {
    if (e.key === 'Enter') handleParse();
});

// ---- 激活逻辑 ----

// 启动时检查激活状态：未激活 → 显示激活面板并锁定主界面
async function checkActivation() {
    try {
        const st = await GetActivationStatus();
        if (st && st.activated) {
            activateWrap.classList.add('hidden');
            return;
        }
        // 未激活：显示面板，填充默认服务器地址
        activateWrap.classList.remove('hidden');
        if (!actServer.value.trim()) {
            actServer.value = st && st.server_url ? st.server_url : 'http://111.170.152.215:18080';
        }
        actCode.focus();
    } catch (err) {
        log(`✗ 激活状态检查失败：${String(err && err.message ? err.message : err)}`, 'err');
        activateWrap.classList.remove('hidden');
    }
}

async function handleActivate() {
    const code = actCode.value.trim();
    if (!code) {
        actMsg.textContent = '请输入激活码';
        actMsg.className = 'act-msg err';
        return;
    }
    // 激活码格式校验：DREC-XXXX-XXXX-XXXX
    if (!/^DREC-[A-Z0-9]{4}-[A-Z0-9]{4}-[A-Z0-9]{4}$/.test(code)) {
        actMsg.textContent = '激活码格式不正确（形如 DREC-XXXX-XXXX-XXXX）';
        actMsg.className = 'act-msg err';
        return;
    }
    // 服务器地址校验：必须是合法 http(s) URL
    let serverURL = actServer.value.trim();
    try {
        const u = new URL(serverURL);
        if (u.protocol !== 'http:' && u.protocol !== 'https:') throw new Error();
        serverURL = u.origin;
    } catch (e) {
        actMsg.textContent = '服务器地址格式不正确（应为 http:// 或 https:// 开头）';
        actMsg.className = 'act-msg err';
        return;
    }
    actBtn.disabled = true;
    actBtn.textContent = '激活中…';
    actMsg.className = 'act-msg';
    actMsg.textContent = '';
    try {
        await Activate(code, serverURL);
        actMsg.textContent = '✓ 激活成功';
        actMsg.className = 'act-msg ok';
        activateWrap.classList.add('hidden');
        log('✓ 激活成功，开始使用', 'ok');
        recBtn.disabled = true;
    } catch (err) {
        actMsg.textContent = `✗ ${String(err && err.message ? err.message : err)}`;
        actMsg.className = 'act-msg err';
    } finally {
        actBtn.disabled = false;
        actBtn.textContent = '激活';
    }
}

actBtn.addEventListener('click', handleActivate);
actCode.addEventListener('keydown', (e) => {
    if (e.key === 'Enter') handleActivate();
});

// 初始禁用录制按钮
recBtn.disabled = true;

// 启动
checkActivation();
