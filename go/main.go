// USB Bridge (Go) -- tray, windowsgui, start-probe (DESIGN §3).
//
// Rev-A behavior:
//   - double-click launch, NO console, NO browser spam during daily use
//   - tray icon menu: panel / re-probe / lock / quit
//   - no vault yet          -> open panel (first-run wizard)
//   - locked (vault exists) -> open panel for unlock
//   - after unlock -> PROBE all enabled channels:
//     any OK      -> silent run (tooltip shows alive count)
//     all FAILED  -> tooltip warns + open the GUI (panel) for fixing
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sync"
	"time"

	"fyne.io/systray"

	"usbridge/internal/audit"
	"usbridge/internal/bridge"
	"usbridge/internal/probe"
	"usbridge/internal/remember"
	"usbridge/internal/server"
	"usbridge/internal/vault"
)

var (
	dataDirFlag = flag.String("data-dir", defaultDataDir(), "USB data folder (default: ./bridge next to exe)")
	portFlag    = flag.Int("port", 0, "listen port override (default from vault, 8989)")
	hostFlag    = flag.String("host", "", "bind address override (default: 127.0.0.1, or 0.0.0.0 when bind_lan set)")
	hiddenFlag  = flag.Bool("hidden", false, "no tray icon at all (B-tier, fully invisible)")
	codexFlag   = flag.Bool("codex", false, "after the gateway unlocks (opening the panel if needed), launch the bundled Codex CLI")
	endpoint    = ""    // http://127.0.0.1:<port>, filled after server up
	silentBoot  = false // true when the remembered-device silent unlock succeeded
)

func defaultDataDir() string {
	exe, err := os.Executable()
	if err != nil {
		return "bridge"
	}
	return filepath.Join(filepath.Dir(exe), "bridge")
}

var openBrowser = func(url string) {
	switch runtime.GOOS {
	case "windows":
		exec.Command("rundll32", "url.dll,FileProtocolHandler", url).Start()
	case "darwin":
		exec.Command("open", url).Start()
	default:
		exec.Command("xdg-open", url).Start()
	}
}

// openBrowserOnce de-duplicates AUTOMATIC panel opens (probe paths can fire
// more than once); the tray menu's "open panel" always uses openBrowser.
var (
	browserOnceMu sync.Mutex
	browserOpened bool
)

func openBrowserOnce(url string) {
	browserOnceMu.Lock()
	defer browserOnceMu.Unlock()
	if browserOpened {
		return
	}
	browserOpened = true
	openBrowser(url)
}

// panelHint: how to reach the panel when we deliberately did NOT auto-open it.
func panelHint() string {
	if silentBoot {
		return "（右键托盘图标 → 打开控制面板）"
	}
	return ""
}

func main() {
	flag.Parse()

	b := bridge.New(*dataDirFlag, *portFlag)

	// re-double-click semantics: an old copy in the tray (still holding the
	// port) is taken down so this launch takes over cleanly.
	takeover := killOldInstances()
	if takeover > 0 {
		fmt.Printf("[bridge] replaced %d old instance(s)\n", takeover)
	}

	var port int
	for i := 0; i < 3; i++ {
		killOldInstances() // same-image instances get cleared every retry
		_, port = server.Run(b, server.Options{DataDir: *dataDirFlag, Host: *hostFlag, PortOverride: *portFlag})
		if port != 0 {
			break
		}
		// port held by a foreign process -- if it is one of OUR gateways
		// (python prototype included), take it down; unknown apps are never killed
		if pid := bridgeSquattingPID(b.Port()); pid > 0 {
			fmt.Printf("[bridge] taking over squatter pid %d (our gateway prototype)\n", pid)
			terminatePID(pid)
			time.Sleep(600 * time.Millisecond)
		}
		time.Sleep(300 * time.Millisecond)
	}
	if port == 0 {
		// port held by an app that is not a usb-bridge instance
		balloonNotify("USB Bridge 启动失败", "端口被其它程序占用，请用 --port 换一个端口")
		time.Sleep(3 * time.Second) // give the balloon a moment to show
		os.Exit(2)
	}
	endpoint = fmt.Sprintf("http://127.0.0.1:%d", port)
	fmt.Printf("[bridge] listening on %s\n", endpoint)

	// Locked-at-boot flow (user spec):
	//   remembered device  -> silent unlock -> PROBE -> bubble "started"; NO browser
	//   never logged in    -> open the panel for password entry (new host)
	if b.Locked() {
		if pw, ok := remember.Load(*dataDirFlag, vault.Path(*dataDirFlag)); ok {
			if _, _, err := b.Unlock(pw); err == nil {
				silentBoot = true
				fmt.Println("[bridge] silent unlock via remembered device")
				go probeAll(b) // bubble only (no browser) on this path
				if *codexFlag {
					go spawnCodex(b)
				}
			} else {
				remember.Clear(*dataDirFlag) // stale blob (password changed?) -> treat as new host
				fmt.Println("[bridge] remembered blob rejected, falling back to panel unlock")
			}
		}
	}
	if b.Locked() {
		openBrowserOnce(endpoint + "/admin") // first-run wizard / new-host unlock
		go waitForUnlockThenProbe(b)
	}
	if *hiddenFlag {
		select {} // B-tier: no tray, no UI
	}
	systray.Run(onReady(b), onExit(b))
}

func onReady(b *bridge.Bridge) func() {
	return func() {
		systray.SetIcon(iconPNG)
		systray.SetTitle("USB Bridge")
		systray.SetTooltip(fmt.Sprintf("USB Bridge -- %s", endpoint))

		open := systray.AddMenuItem("打开控制面板", "在浏览器中打开控制面板")
		reprobe := systray.AddMenuItem("重新测活渠道", "再次测试所有上游渠道")
		codexItem := systray.AddMenuItem("启动 Codex", "检测解锁后启动 U 盘内置的 Codex CLI（未登录会先打开登录页）")
		lockItem := systray.AddMenuItem("锁定（暂停服务）", "锁定后所有下游调用 503，直到重新解锁")
		systray.AddSeparator()
		quit := systray.AddMenuItem("退出", "保存用量并退出")

		go func() {
			for {
				select {
				case <-open.ClickedCh:
					openBrowser(endpoint + "/admin")
				case <-reprobe.ClickedCh:
					go probeAll(b)
				case <-codexItem.ClickedCh:
					go launchCodex(b)
				case <-lockItem.ClickedCh:
					b.Lock()
					systray.SetIcon(iconLockedPNG)
					systray.SetTooltip(fmt.Sprintf("USB Bridge locked -- %s/admin", endpoint))
				case <-quit.ClickedCh:
					systray.Quit()
				}
			}
		}()
	}
}

func onExit(b *bridge.Bridge) func() {
	return func() {
		if v := b.Vault(); v != nil {
			v.Save()
		}
		if a := b.Audit(); a != nil {
			a.Close()
		}
		fmt.Println("[bridge] flushed and exited")
	}
}

func waitForUnlockThenProbe(b *bridge.Bridge) {
	for {
		time.Sleep(500 * time.Millisecond)
		if !b.Locked() {
			break
		}
	}
	probeAll(b)
	if *codexFlag { // 「打开网页登录 → 解锁瞬间自动打开 Codex」
		spawnCodex(b)
	}
}

// launchCodex runs from the tray menu: unlock first if needed, then spawn.
func launchCodex(b *bridge.Bridge) {
	if b.Locked() {
		openBrowserOnce(endpoint + "/admin")
		for b.Locked() {
			time.Sleep(500 * time.Millisecond)
		}
	}
	spawnCodex(b)
}

// spawnCodex starts the bundled Codex CLI (bin/codex.exe next to us) with
// CODEX_HOME on the USB stick -- chat history and config never touch the host.
// A console subprocess of a windowsgui app gets its own terminal window.
func spawnCodex(b *bridge.Bridge) {
	exe, err := os.Executable()
	if err != nil {
		return
	}
	codexExe := filepath.Join(filepath.Dir(exe), "bin", "codex.exe")
	if _, err := os.Stat(codexExe); err != nil {
		balloonNotify("Codex 未找到", "U 盘缺少 bin\\codex.exe：用 publish.ps1 重新打包，或从 openai/codex Releases 下载放入")
		return
	}
	home := filepath.Join(*dataDirFlag, "codex")
	env := append(os.Environ(), "CODEX_HOME="+home)
	// belt and braces: some codex builds resolve env_key strictly from the
	// environment, so export the key alongside auth.json
	if raw, err := os.ReadFile(filepath.Join(home, "auth.json")); err == nil {
		var m map[string]string
		if json.Unmarshal(raw, &m) == nil && m["OPENAI_API_KEY"] != "" {
			env = append(env, "OPENAI_API_KEY="+m["OPENAI_API_KEY"])
		}
	}
	cmd := exec.Command(codexExe)
	cmd.Env = env
	if err := cmd.Start(); err != nil {
		balloonNotify("Codex 启动失败", err.Error())
		return
	}
	balloonNotify("Codex 已启动", "Codex CLI 窗口已打开；网关与历史都保存在 U 盘")
}

// probeAll: start-probe per rev-A spec.
//
//   - silentBoot (remembered device): NEVER auto-open the browser; problems are
//     reported with a tray bubble only (the tray menu has 打开控制面板).
//   - interactive paths (first run / new host): a failing probe opens the panel.
func probeAll(b *bridge.Bridge) {
	if b.Locked() {
		systray.SetTooltip(fmt.Sprintf("USB Bridge locked -- %s/admin", endpoint))
		openBrowserOnce(endpoint + "/admin")
		return
	}
	pol := b.Policy()
	chs := b.Sched().All()
	alive := 0
	total := 0
	for _, ch := range chs {
		if !ch.Enabled {
			continue
		}
		total++
		res := probe.Channel(ch, pol.TimeoutFirstByteS)
		b.Sched().Report(ch.ID, res.OK, res.FirstMS, res.Status, 0)
		if a := b.Audit(); a != nil {
			a.Record(audit.Record{
				Model: "probe:" + ch.ID, ChannelID: ch.ID,
				Status: res.Status, FirstByteMS: res.FirstMS, Error: res.Err,
			})
		}
		if res.OK {
			alive++
		}
	}
	probe.SetAggregate(total, alive)
	if total == 0 {
		systray.SetTooltip(fmt.Sprintf("USB Bridge running on %s -- no channels configured", endpoint))
		if silentBoot {
			balloonNotify("USB Bridge 已启动（未配置渠道）", "还没有渠道，右键托盘图标 → 打开控制面板添加")
			return
		}
		balloonNotify("USB Bridge 未配置渠道", "还没有渠道，已打开控制面板添加")
		openBrowserOnce(endpoint + "/admin")
		return
	}
	if alive == 0 {
		// probe failed -> bubble; auto-open the GUI only on non-silent boots
		systray.SetTooltip(fmt.Sprintf("USB Bridge -- 0/%d channels alive, open panel to fix", total))
		balloonNotify("USB Bridge 渠道全部无响应", fmt.Sprintf("%d 个渠道全部无响应%s", total, panelHint()))
		fmt.Printf("[probe] 0/%d channels alive\n", total)
		if !silentBoot {
			openBrowserOnce(endpoint + "/admin")
		}
		return
	}
	systray.SetIcon(iconPNG)
	systray.SetTooltip(fmt.Sprintf("USB Bridge running on %s -- channels alive %d/%d", endpoint, alive, total))
	balloonNotify("USB Bridge 启动成功", fmt.Sprintf("%d/%d 渠道可用 · 端口 %d · 已就绪", alive, total, b.Port()))
	fmt.Printf("[probe] %d/%d channels alive\n", alive, total)
}
