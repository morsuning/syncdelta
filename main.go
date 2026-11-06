// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at https://mozilla.org/MPL/2.0/.

package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"runtime/debug"
	"strings"
	"time"
)

// 构建时通过 -ldflags "-X main.version=xxx" 注入
var version = "dev"

func main() {
	dataFlag := flag.String("data-dir", "", "数据目录(配置、基线、日志)。默认:可执行文件所在目录下的 syncdelta-data 文件夹")
	portFlag := flag.Int("port", 0, "监听端口(仅本机 127.0.0.1),默认随机空闲端口")
	noBrowser := flag.Bool("no-browser", false, "启动后不自动打开浏览器")
	noAutoExit := flag.Bool("no-auto-exit", false, "关闭网页后不自动退出")
	showVer := flag.Bool("version", false, "显示版本号")
	flag.Parse()
	if *showVer {
		fmt.Println("syncdelta", version)
		return
	}

	dataDir, err := resolveDataDir(*dataFlag)
	if err != nil {
		fatal(err.Error())
	}
	setupLog(dataDir)
	log.Printf("syncdelta %s %s/%s, data dir: %s", version, runtime.GOOS, runtime.GOARCH, dataDir)

	// 已经有一个实例在运行:直接让浏览器打开它,然后退出
	if u := runningInstanceURL(dataDir); u != "" {
		log.Printf("already running at %s", u)
		fmt.Println("SyncDelta 已在运行:", u)
		if !*noBrowser {
			openURL(u)
		}
		return
	}

	store, err := OpenStore(dataDir)
	if err != nil {
		fatal("无法读取配置: " + err.Error())
	}
	ln, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", *portFlag))
	if err != nil {
		fatal("无法监听本地端口: " + err.Error())
	}
	port := ln.Addr().(*net.TCPAddr).Port
	tb := make([]byte, 16)
	_, _ = rand.Read(tb)

	app := &App{dataDir: dataDir, token: hex.EncodeToString(tb), port: port, store: store,
		job: &Job{}, life: newLifecycle(), quit: make(chan struct{})}
	srv := &http.Server{Handler: app.routes(), ReadHeaderTimeout: 10 * time.Second}
	go func() {
		if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Printf("server error: %v", err)
			app.requestQuit()
		}
	}()

	url := fmt.Sprintf("http://127.0.0.1:%d/?t=%s", port, app.token)
	writeInstance(dataDir, port, app.token)
	defer os.Remove(filepath.Join(dataDir, "instance.json"))
	fmt.Println("SyncDelta 已启动:", url)
	log.Printf("listening on 127.0.0.1:%d", port)

	if !*noAutoExit {
		go app.watch()
	}
	if !*noBrowser {
		openURL(url)
	}

	sig := make(chan os.Signal, 1)
	signal.Notify(sig, os.Interrupt)
	select {
	case <-sig:
	case <-app.quit:
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	_ = srv.Shutdown(ctx)
	log.Printf("bye")
}

func fatal(msg string) {
	log.Print("FATAL: ", msg)
	showError(msg)
	os.Exit(1)
}

func logPanic(r any) { log.Printf("panic: %v\n%s", r, debug.Stack()) }

// ---------------------------------------------------------------- 数据目录 / 日志

// 所有数据(任务配置、基线、日志)都放在可执行文件所在目录下的 syncdelta-data 文件夹里,
// 不使用任何系统目录,把程序连同这个文件夹一起拷走就能带走全部状态。
func resolveDataDir(flagVal string) (string, error) {
	dir := flagVal
	if dir == "" {
		exe, err := os.Executable()
		if err != nil {
			return "", fmt.Errorf("无法确定程序所在目录: %w", err)
		}
		if real, err := filepath.EvalSymlinks(exe); err == nil {
			exe = real // 通过符号链接启动时,以真实文件位置为准
		}
		dir = filepath.Join(filepath.Dir(exe), "syncdelta-data")
	}
	dir, err := filepath.Abs(dir)
	if err != nil {
		return "", err
	}
	hint := fmt.Errorf("无法在程序所在位置写入数据文件夹:\n%s\n\n请把程序放到有写入权限的文件夹里再运行(不要放在只读位置,例如 Windows 的 Program Files、只读 U 盘),或用 --data-dir 指定其他位置。", dir)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", fmt.Errorf("%w\n(%v)", hint, err)
	}
	// MkdirAll 对已存在的只读目录也会成功,所以再实际写一次来确认
	probe := filepath.Join(dir, ".write-test")
	if err := os.WriteFile(probe, nil, 0o644); err != nil {
		return "", fmt.Errorf("%w\n(%v)", hint, err)
	}
	_ = os.Remove(probe)
	return dir, nil
}

func setupLog(dataDir string) {
	p := filepath.Join(dataDir, "syncdelta.log")
	if fi, err := os.Stat(p); err == nil && fi.Size() > 1<<20 {
		_ = os.Rename(p, p+".old")
	}
	f, err := os.OpenFile(p, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return
	}
	// 文件放在前面:Windows GUI 程序没有 stderr,写失败不能影响写文件
	log.SetOutput(io.MultiWriter(f, os.Stderr))
}

// ---------------------------------------------------------------- 单实例

type instanceInfo struct {
	Port  int    `json:"port"`
	Token string `json:"token"`
	PID   int    `json:"pid"`
}

func writeInstance(dataDir string, port int, token string) {
	b, _ := json.Marshal(instanceInfo{port, token, os.Getpid()})
	_ = os.WriteFile(filepath.Join(dataDir, "instance.json"), b, 0o600)
}

func runningInstanceURL(dataDir string) string {
	b, err := os.ReadFile(filepath.Join(dataDir, "instance.json"))
	if err != nil {
		return ""
	}
	var ii instanceInfo
	if json.Unmarshal(b, &ii) != nil || ii.Port == 0 {
		return ""
	}
	c := http.Client{Timeout: time.Second}
	resp, err := c.Get(fmt.Sprintf("http://127.0.0.1:%d/healthz", ii.Port))
	if err != nil {
		return ""
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 32))
	if strings.TrimSpace(string(body)) != "syncdelta" {
		return ""
	}
	return fmt.Sprintf("http://127.0.0.1:%d/?t=%s", ii.Port, ii.Token)
}

// ---------------------------------------------------------------- 打开浏览器 / 文件夹

func openURL(url string) {
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "darwin":
		cmd = exec.Command("open", url)
	case "windows":
		cmd = exec.Command("rundll32", "url.dll,FileProtocolHandler", url)
	default:
		cmd = exec.Command("xdg-open", url)
	}
	hideWindow(cmd)
	if err := cmd.Start(); err != nil {
		log.Printf("cannot open browser: %v (open this URL manually: %s)", err, url)
		return
	}
	go func() { _ = cmd.Wait() }()
}

func revealDir(dir string) error {
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "darwin":
		cmd = exec.Command("open", dir)
	case "windows":
		cmd = exec.Command("explorer", dir)
	default:
		cmd = exec.Command("xdg-open", dir)
	}
	hideWindow(cmd)
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("无法打开文件夹: %w", err)
	}
	go func() { _ = cmd.Wait() }() // explorer 总是返回非 0,忽略
	return nil
}
