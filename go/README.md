# USB Bridge -- Go 正式版（A 档）

单文件 exe，双击即托盘运行：无控制台黑框、不乱开网页，插 U 盘即用。
`DESIGN.md` 的 Go 落地版；加密配置与 Python 原型**二进制互通**。

## 产物

| 文件 | 用途 |
|---|---|
| `usb-bridge.exe` | 正式版（`-ldflags "-H windowsgui"`，无黑框，托盘图标） |
| `usbgw.exe`（e2e 构建） | 控制台开发版，用于测试/排障 |

## 行为（A 档 + 启动探针 + 记住本机）

1. **双击启动**：无控制台。托盘出现品牌图标——与网页面板 favicon
   **同一套几何与渐变**（深色圆角方块 + 「桥」字形描边），运行时按
   16/32/48/256 逐档位图渲染后打包成多尺寸 ICO（锁定态换成琥珀色同款）。
   **重复双击 = 自动退出托盘里的旧实例并由新实例接管**（强杀安全：配置落盘是
   tmp+rename 原子写；端口若被非本程序占用会气泡提示后退出）。
2. **首次运行**（无 vault）：自动打开浏览器面板 `http://127.0.0.1:8989/admin` 走创建向导。
3. **解锁页有「记住这台设备」**（默认勾选）：勾上后主密码以 **DPAPI**
   （Windows 当前用户加密）存在本机 `bridge/session.key`——U 盘拔到别的电脑、
   换别的 Windows 账户都解不开；`config.vault` 落盘永远是密文。
4. **曾经登录过的电脑**：双击 → 静默解锁 → 自动探针 →
   **托盘气泡“USB Bridge 启动成功”（n/m 渠道可用），全程不开浏览器、不输密码**；
   探针有问题也只气泡提醒（不再自动弹浏览器），要排查右键托盘 →「打开控制面板」。
   只有**新电脑**（没记住过的机器）才会弹面板要一次主密码。
5. 托盘菜单：**打开控制面板 / 重新测活渠道 / 锁定 / 退出**。退出前自动 flush 配置与用量。
6. **已解锁时面板免密**：网关已静默解锁的情况下，本机浏览器打开面板会通过
   `GET /admin/token`（**仅接受 127.0.0.1 回环请求**）自动拿到本次会话令牌，
   省去再输一遍主密码；「锁定」后该令牌立刻失效，面板重新要密码。
   局域网/手机浏览器永远拿不到管理令牌（该接口对非回环请求返回 403）。

## 面板

单文件内嵌页面（`internal/webui/page.html`），深色卡片式界面：最上方「快速接入」
（OpenAI 兼容地址 / Anthropic 原生地址 / 局域网地址 / 下游 key / 模型 chips 一键复制），
下面是 24 小时统计、渠道、下游 key、设置、最近请求。手机浏览器自适应。面板响应带
`Cache-Control: no-store`，换 exe 版本刷新即见新界面。

## 改密码

面板「设置」卡里直接改：输入当前主密码 + 新密码，即时生效（vault 用新密钥重加密，
记住本机的 vault 同步换血）。主密码不锁下游 key（`sk-usb-...` 不受影响）。
Python 原型的老 vault 也可用 `python python/bridge.py passwd` 改。

## 手机端能用

前置：**网关默认只听 127.0.0.1，手机连不上**——这是刻意的安全默认。
要手机用，打开面板「设置 → 允许局域网/手机访问」（监听 0.0.0.0，重启生效）：

- **同一 WiFi**：手机 App（ChatBox/NextChat/LobeChat 等任何可自定义
  base_url 的 OpenAI 兼容客户端）填 `http://<电脑IP>:8989/v1` + 下游 key 即可；
  手机浏览器开 `http://<电脑IP>:8989/admin` 也能直接管理。注意是纯 HTTP（局域网内），
  下游 key 是唯一门禁，别在公共 WiFi 用。
- **外网或在途中**：装 [Tailscale](https://tailscale.com)（电脑+手机各一），
  手机填 `http://<电脑的tailscale IP>:8989/v1` —— WireGuard 加密直连，
  无需端口映射、不需要 bind_lan（tailnet 指电脑的 100.x 地址也要监听对应网卡，
  一样开 LAN 或 `--host 100.x.x.x`）。

## 对外端点（双协议）

一律用同一个下游 key（`sk-usb-…`，Bearer 或 x-api-key 均收）：

| 端点 | 协议 | 用途 |
|---|---|---|
| `POST /v1/chat/completions` | OpenAI | 绝大多数客户端 |
| `POST /v1/messages` | Anthropic 原生 | Claude 官方 SDK / Claude 语法的 App |
| `GET /v1/models` | OpenAI | 模型列表 |
| `POST /v1/embeddings` | OpenAI | 向量 |
| `ANY /proxy/<channel_id>` | 通道原协议 | 原样反代到上游指定渠道 |

`/v1/messages` 支持流式（message_start → content_block_delta → message_stop）；
模型在 OpenAI 协议渠道上时网关双向转换，Anthropic 渠道原样转发。

面板「快速接入」卡（页面最上方）把这两组 base_url、下游 key、全部模型
chips 做成一键复制，局域网开启时还会附上 `http://<LAN IP>:8989` 地址。

管理接口（本机面板用）：`GET /admin/state`（公开，仅供面板判断锁态）、
`POST /admin/unlock`、`POST /admin/cmd`（需 `X-Admin-Token`）、
`GET /admin/token`（**仅回环**：已解锁时发本次会话令牌，供本机面板免密进入）。

## 使用

```
usb-bridge.exe                     # 数据目录 = exe 同目录的 bridge\
usb-bridge.exe --data-dir D:\bridge --port 8989
usb-bridge.exe --hidden            # B 档：完全无托盘的纯后台进程
```

下游接入方式与 Python 版一致：`base_url = http://127.0.0.1:8989/v1`，
API key 用面板生成的 `sk-usb-...`（OpenAI 兼容），被截胡的 Anthropic
请求会自动做协议转换。`/proxy/{channel_id}/path` 反代透传照旧。

- **复用已有配置**：把原型数据目录指给 Go 版即可
  `usb-bridge.exe --data-dir "...\usb-token-gateway\python\bridge"`，
  主密码不变（Go 可读原型的 argon2 与 scrypt 两种加密后端）；
  Go 写入的新 vault 为 argon2id 格式（老 Python 需 cryptography>=43 才能读）。
- 面板接口、curl 示例均与 `README.md` 顶层文档相同。

## 构建 / 测试

```
go test ./...                     # 单元测试（图标编码/几何、Windows HICON 加载）
go build -o usbgw.exe .           # 控制台开发版
go build -trimpath -ldflags "-H windowsgui -s -w" -o usb-bridge.exe .

python ..\tests\e2e_go.py         # 78 项端到端（含跨语言 vault 互通 + 记住本机链路）
python ..\tests\e2e_test.py       # Python 原型回归 31 项
```

一键构建（自动定位 go，产物直接进 `usb\`）：`powershell -ExecutionPolicy Bypass -File ..\build.ps1`。

e2e 覆盖：跨语言解锁（Go 解 Python 的 scrypt vault 并正常代理）、
argon2 新建/重锁/错误密码、SSE 双协议转换与拼装、failover、未知模型 503、
embeddings、/proxy、admin 全命令、探针 test_channel、锁屏 503、重启仍锁定、
vault 落盘恒为密文。

## 结构

```
main.go            入口：托盘 + 启动探针 + 浏览器开面板
icon.go            运行时渲染品牌图标（与网页 favicon 同几何 + 多尺寸 ICO 封装）
icon_windows.go    HICON 加载（托盘与气泡通知共用同一枚图标）
internal/vault/    加密配置（argon2id 写 / 读双后端）
internal/bridge/   网关状态机（锁 / 解锁 / 管理令牌）
internal/sched/    调度（4 策略 + 5 分钟健康窗 + 熔断 + 429 冷却）
internal/xfmt/     OpenAI ↔ Anthropic 转换（含 SSE 事件映射）
internal/server/   HTTP 网关（/v1/*、/proxy、/admin、SSE、panic 恢复）
internal/probe/    探针（启动探针 + admin test_channel 共用）
internal/audit/    用量 JSONL（bridge/audit/log-YYYYMMDD.jsonl）+ 内存环
internal/webui/    控制面板（embed 复用原型单页 HTML）
```
