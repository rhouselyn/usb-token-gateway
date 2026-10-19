# -*- coding: utf-8 -*-
"""Control panel (DESIGN §8): single embedded HTML page.

Flow: /admin/state (public) -> unlock-or-init card -> dashboard.
Every data endpoint requires the admin token returned by unlock.
"""

from __future__ import annotations

PAGE = r"""<!doctype html>
<html lang="zh-CN"><head><meta charset="utf-8">
<title>USB Bridge 控制台</title>
<meta name="viewport" content="width=device-width, initial-scale=1">
<style>
  body{font-family:system-ui,'Segoe UI',sans-serif;margin:24px;background:#0d1117;color:#e6edf3}
  h1{font-weight:600} h2{font-size:16px;margin:4px 0 10px}
  a{color:#58a6ff}
  table{border-collapse:collapse;margin:10px 0;width:100%}
  th,td{border:1px solid #30363d;padding:6px 10px;text-align:left;font-size:13px}
  th{background:#161b22}
  .ok{color:#3fb950}.bad{color:#f85149}.muted{color:#8b949e}
  input,select{background:#161b22;color:#e6edf3;border:1px solid #30363d;border-radius:6px;padding:7px 10px;margin:3px 6px 3px 0;font-size:13px}
  button{background:#21262d;color:#e6edf3;border:1px solid #30363d;border-radius:6px;padding:7px 12px;margin:3px 6px 3px 0;cursor:pointer;font-size:13px}
  button:hover{background:#30363d} button.danger:hover{background:#5c1a1a}
  section{margin:20px 0;padding:14px 16px;border:1px solid #30363d;border-radius:10px}
  .tag{display:inline-block;padding:3px 10px;border-radius:10px;background:#21262d;font-size:12px;margin:2px 6px 2px 0}
  .card{max-width:420px;margin:60px auto;padding:28px;border:1px solid #30363d;border-radius:12px;background:#161b22}
  .card h1{font-size:20px} .card p{color:#8b949e;font-size:13px}
  .card input{width:92%}
  .rowline{display:flex;align-items:center;gap:6px;flex-wrap:wrap}
  #banner{position:sticky;top:0;z-index:5;background:#0d1117;padding:6px 0}
  pre{background:#010409;padding:8px;border-radius:6px;font-size:12px;overflow:auto}
</style></head><body>

<div id="gate"></div>

<div id="app" style="display:none">
<div id="banner">
  <h1 style="font-size:20px; display:inline">&#128190; USB Bridge 控制台</h1>
  <span class="tag" id="b_port"></span>
  <span class="tag" id="b_strategy"></span>
  <span class="tag ok" id="b_state">运行中</span>
  <button class="danger" onclick="lockNow()">🔒 锁屏（停止服务直到重新解锁）</button>
</div>

<section><h2>&#128200; 近 24 小时</h2><div id="summary"></div></section>

<section><h2>&#128278; 渠道（上游 API）</h2><div id="channels"></div>
  <details><summary>&#10133; 添加渠道</summary><div class="rowline">
    <select id="ch_proto">
      <option value="openai">openai / OpenAI兼容</option>
      <option value="anthropic">anthropic / Claude</option>
      <option value="gemini">gemini（M2支持）</option>
    </select>
    <input id="ch_base" placeholder="base_url，如 https://api.openai.com/v1" size="42">
    <input id="ch_key" placeholder="上游 API Key" size="26">
    <input id="ch_models" placeholder="models（逗号分隔，支持*通配）" size="34">
    <input id="ch_weight" type="number" value="10" style="width:70px" title="权重">
    <input id="ch_priority" type="number" value="0" style="width:70px" title="优先级（failover顺序，越小越优先）">
    <button onclick="addChannel()">添加</button>
  </div></details>
</section>

<section><h2>&#128273; 下游私有 Key（发给你的项目用）</h2><div id="dkeys"></div>
  <div class="rowline">
    <input id="dk_name" placeholder="备注名" size="14">
    <input id="dk_ttl" type="number" placeholder="有效期(小时,0=永久)" style="width:150px">
    <button onclick="genKey()">➕ 生成新 Key</button>
  </div>
</section>

<section><h2>&#9881; 策略与端口</h2><div class="rowline">
  轮换策略
  <select id="strategy">
    <option value="failover">failover（优先级+故障转移，推荐）</option>
    <option value="round_robin">round_robin（均匀轮询）</option>
    <option value="weighted">weighted（加权随机）</option>
    <option value="least_errors">least_errors（错误最少优先）</option>
  </select>
  <button onclick="setStrategy()">应用</button>
  &nbsp;&nbsp;监听端口 <input id="port" type="number" style="width:90px">
  <button onclick="setPort()">保存端口（重启生效）</button>
</div></section>

<section><h2>&#128203; 最近请求（实时）</h2>
  <div class="rowline"><button onclick="refresh()">🔄 刷新</button>
  <span class="muted">网关退出或锁屏会停止所有下游调用；日志只在本页展示。</span></div>
  <div id="recent"></div>
</section>
</div>

<script>
let TOKEN = sessionStorage.getItem('brg-tok') || '';

function hh(){ return {'X-Admin-Token': TOKEN, 'Content-Type': 'application/json'}; }
async function jget(p){ const r = await fetch('/admin/'+p, {headers: hh()}); return r; }
async function jpost(action, data){
  const r = await fetch('/admin/cmd', {method:'POST', headers: hh(), body: JSON.stringify({action, ...data})});
  if (r.status === 401){ TOKEN=''; sessionStorage.removeItem('brg-tok'); boot(); throw new Error('token失效，请重新解锁'); }
  return r.json();
}
function h(s){ return String(s).replace(/[&<>"]/g, c=>({'&':'&amp;','<':'&lt;','>':'&gt;','"':'&quot;'}[c])); }
function mask(k){ return k ? k.slice(0,10) + '****' + k.slice(-4) : '-'; }

/* 揭示/复制 key 的状态与工具 */
const revealKeys = new Set();          // 展开明文的 key
let toastTimer = null;
function toast(msg){
  let t = document.getElementById('brg-toast');
  if (!t){ t = document.createElement('div'); t.id = 'brg-toast';
    t.style.cssText = 'position:fixed;bottom:24px;left:50%;transform:translateX(-50%);'
      + 'background:#1f6feb;color:#fff;padding:10px 18px;border-radius:8px;font-size:14px;'
      + 'z-index:99;box-shadow:0 4px 16px rgba(0,0,0,.5)'; document.body.appendChild(t); }
  t.textContent = msg; t.style.display = 'block';
  clearTimeout(toastTimer); toastTimer = setTimeout(()=>{ t.style.display='none'; }, 2200);
}
async function copyKey(k){
  try{ await navigator.clipboard.writeText(k); toast('✔ 已复制到剪贴板'); }
  catch(e){ // 非安全上下文(纯http)兜底: 隐藏textarea + execCommand
    const ta = document.createElement('textarea'); ta.value = k;
    ta.style.cssText = 'position:fixed;opacity:0'; document.body.appendChild(ta);
    ta.select(); document.execCommand('copy'); ta.remove(); toast('✔ 已复制到剪贴板');
  }
}

/* ---------------------------------------------- boot: gate or dashboard */
async function boot(){
  const st = await fetch('/admin/state').then(r=>r.json());
  if (!st.locked && TOKEN){ showApp(); refresh(); return; }
  renderGate(st);
}

function renderGate(st){
  const gate = document.getElementById('gate');
  const fresh = !st.has_vault;
  gate.innerHTML = `<div class="card">
    <h1>&#128190; USB Bridge</h1>
    <p>${fresh ? '首次使用：设置一个主密码，它加密保存你的所有 API key。<b>主密码丢了无法找回配置！</b>'
               : '本机的加密配置已存在，输入主密码解锁本次会话。'}</p>
    <input id="gp1" type="password" placeholder="主密码" onkeydown="gateEnter(event)">
    ${fresh ? '<input id="gp2" type="password" placeholder="再输一遍确认" onkeydown="gateEnter(event)">' : ''}
    <button onclick="unlock()">&#128273; ${fresh ? '创建配置' : '解锁'}</button>
    <p class="muted">端口 ${st.port} · ${fresh ? '创建成功后自动进入控制台' : '解锁后进入控制台'}</p>
  </div>`;
}

async function gateEnter(e){ if (e.key === 'Enter') unlock(); }

async function unlock(){
  const p1 = document.getElementById('gp1').value;
  const p2v = document.getElementById('gp2');
  if (p2v && p1 !== p2v.value){ toast('两次密码不一致'); return; }
  try{
    const r = await fetch('/admin/unlock', {method:'POST', headers:{'Content-Type':'application/json'},
      body: JSON.stringify({master_password: p1})});
    const j = await r.json();
    if (!j.ok){ toast('解锁失败: ' + (j.error||'')); return; }
    TOKEN = j.admin_token; sessionStorage.setItem('brg-tok', TOKEN);
    if (j.fresh) getFirstKey(j);
    showApp(); refresh();
  }catch(e){ toast(String(e)); }
}

async function getFirstKey(_j){
  // 首次创建后把主 key 展开显示（表格里直接可复制，不再弹窗）
  try{ const r = await jget('dkeys'); const ks = await r.json();
    if (ks.keys && ks.keys.length) revealKeys.add(ks.keys[0].key);
  }catch(e){}
}

function reveal(k){
  if (revealKeys.has(k)) revealKeys.delete(k); else revealKeys.add(k);
  refresh();
}

async function lockNow(){
  if (!confirm('锁屏后所有下游调用都会 503，需要重新输入主密码。继续？')) return;
  try{ await jpost('lock'); TOKEN=''; sessionStorage.removeItem('brg-tok'); boot(); }catch(e){ alert(e); }
}

function showApp(){ document.getElementById('gate').innerHTML=''; document.getElementById('app').style.display='block'; }

/* ---------------------------------------------- dashboard */
async function refresh(){
  try{
    const r1 = await jget('summary'); if(!r1.ok) throw new Error('unauthorized'); const s = await r1.json();
    const chs = await (await jget('channels')).json();
    const dks = await (await jget('dkeys')).json();
    const rec = await (await jget('recent')).json();
    const st = await fetch('/admin/state').then(r=>r.json());

    document.getElementById('b_port').textContent = '端口 ' + st.port;
    document.getElementById('b_strategy').textContent = '策略: ' + s.strategy;
    document.getElementById('b_state').textContent = '运行中 · 渠道 ' + chs.channels.filter(c=>c.enabled).length + '/' + chs.channels.length;
    document.getElementById('port').value = st.port;
    document.getElementById('strategy').value = s.strategy;

    document.getElementById('summary').innerHTML =
      `<span class="tag">请求 ${s.requests_24h}</span>
       <span class="tag bad">错误 ${s.errors_24h}</span>
       <span class="tag">tokens 上行 ${s.tokens_24h.prompt} / 下行 ${s.tokens_24h.completion}</span>`
      + s.by_channel.map(c=>`<span class="tag">${h(c.channel)}: ${c.requests}req · ${c.tokens}tok</span>`).join('');

    document.getElementById('channels').innerHTML = chs.channels.length ? table(
      ['id','协议','models','状态','key','操作'],
      chs.channels.map(c=>`<tr><td class="muted">${h(c.id)}</td><td>${h(c.protocol)}</td>
        <td>${h((c.models||[]).join(', '))}</td>
        <td class="${c.enabled?'ok':'muted'}">${c.enabled?'● 启用':'○ 停用'}</td>
        <td class="muted">${h(c.key_masked)}</td>
        <td><button onclick="testChannel('${h(c.id)}')">测活</button>
            <button onclick="cmd('toggle_channel','${h(c.id)}')">${c.enabled?'停用':'启用'}</button>
            <button class="danger" onclick="cmd('del_channel','${h(c.id)}')">删除</button></td></tr>`)
      ) : '<span class="muted">还没有渠道 —— 点上面「添加渠道」把你的 API key 加进来</span>';

    document.getElementById('dkeys').innerHTML = table(
      ['名称','key','状态','有效期','操作'],
      dks.keys.map(k=>{
        const shown = revealKeys.has(k.key);
        return `<tr><td>${h(k.name)}</td>
        <td${shown ? '' : ' class="muted"'}>${shown ? h(k.key) : h(k.key_masked)}</td>
        <td class="${k.enabled?'ok':'muted'}">${k.enabled?'✓':'✗'}</td>
        <td>${k.ttl_hours? k.ttl_hours+' 小时（'+fmtTtl(k)+'）':'永久'}</td>
        <td><button onclick="copyKey('${h(k.key)}')">📋 复制</button>
            <button onclick="reveal('${h(k.key)}')">${shown?'🙈 收起':'👁 显示'}</button>
            <button onclick="cmd('toggle_dkey','${h(k.key)}')">${k.enabled?'停用':'启用'}</button>
            <button class="danger" onclick="cmd('del_dkey','${h(k.key)}')">删除</button></td></tr>`;
      })
      ,'<span class="muted">暂无 key —— 点下面「生成新 Key」，生成后可直接复制</span>');

    document.getElementById('recent').innerHTML = table(
      ['时间','model','渠道','状态','首字ms','tokens','错误'],
      rec.recent.slice(0,40).map(r=>`<tr>
        <td class="muted">${new Date(r.ts*1000).toLocaleTimeString()}</td>
        <td>${h(r.model||'')}</td><td>${h(r.channel_id||'-')}</td>
        <td class="${(r.status>=400||r.status==0)?'bad':'ok'}">${r.status}</td>
        <td>${Math.round(r.first_byte_ms||0)}</td>
        <td>${r.prompt_tokens}/${r.completion_tokens}</td>
        <td class="muted">${h((r.error||'').slice(0,60))}</td></tr>`).join('')
      ,'<span class="muted">暂无请求</span>');
  }catch(e){
    console.error(e);
    document.getElementById('summary').innerHTML = `<span class="bad">${h(String(e))}</span>
      <span class="muted">（若提示 401：请刷新页面重新输入主密码解锁）</span>`;
  }
}
function table(head, rowsHtml, empty){
  const rows = Array.isArray(rowsHtml) ? rowsHtml.join('') : (rowsHtml || '');
  if (!rows || !rows.length) return empty || '';
  return `<table><tr>${head.map(t=>'<th>'+t+'</th>').join('')}</tr>${rows}</table>`;
}
function fmtTtl(k){ return '到 ' + new Date((k.created||0)*1000 + (k.ttl_hours||0)*3600e3).toLocaleString(); }

/* ---------------------------------------------- actions */
async function cmd(action, arg){ const j = await jpost(action, {arg}); if (!j.ok) alert('失败: ' + (j.error||'')); refresh(); }
async function genKey(){
  const j = await jpost('add_dkey', {name: document.getElementById('dk_name').value, ttl: +document.getElementById('dk_ttl').value||0});
  if (!j.ok){ toast('✘ 生成失败: ' + (j.error||'')); return; }
  if (j.new_key){ revealKeys.add(j.new_key); toast('✔ 已生成，key 已展开 —— 点行内 📋 复制'); }
  refresh();
}
async function addChannel(){
  const j = await jpost('add_channel', {
    proto: document.getElementById('ch_proto').value,
    base_url: document.getElementById('ch_base').value.trim(),
    api_key: document.getElementById('ch_key').value.trim(),
    models: document.getElementById('ch_models').value.trim() || '*',
    weight: +document.getElementById('ch_weight').value||10,
    priority: +document.getElementById('ch_priority').value||0,
  });
  if (!j.ok) alert('添加失败: ' + (j.error||''));
  else { ['ch_base','ch_key','ch_models'].forEach(id=>document.getElementById(id).value=''); }
  refresh();
}
async function setStrategy(){ cmd('set_strategy', document.getElementById('strategy').value); }
async function setPort(){ cmd('set_port', +document.getElementById('port').value||8989); }
async function testChannel(cid){
  const j = await jpost('test_channel', {arg: cid});
  const msg = j.ok ? `✔ ${cid} 连通\n状态 ${j.status} · 首字节 ${j.first_byte_ms}ms\n\n${j.body||''}`
                   : `✘ ${cid}\n状态 ${j.status||'-'}\n${j.error||j.body||''}`;
  alert(msg);
}

boot();
</script>
</body></html>
"""


def render_page() -> bytes:
    return PAGE.encode()


def sanitize_channel(ch: dict) -> dict:
    key = ch.get("api_key", "") or ""
    return {
        "id": ch.get("id"),
        "protocol": ch.get("protocol"),
        "base_url": ch.get("base_url"),
        "models": ch.get("models", []),
        "enabled": bool(ch.get("enabled", True)),
        "weight": ch.get("weight"),
        "priority": ch.get("priority"),
        "key_masked": (key[:8] + "****" + key[-4:]) if len(key) > 10 else (key[:4] + "****" if key else "-"),
    }


def sanitize_dkey(dk: dict) -> dict:
    key = dk.get("key", "") or ""
    return {
        "name": dk.get("name"),
        "key": key,  # masked in JS; needed for copy-after-generate flows
        "key_masked": key[:10] + "****" + key[-4:] if len(key) > 14 else key[:4] + "****",
        "enabled": bool(dk.get("enabled", True)),
        "ttl_hours": dk.get("ttl_hours", 0),
        "created": dk.get("created", 0),
    }
