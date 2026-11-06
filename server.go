// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at https://mozilla.org/MPL/2.0/.

package main

import (
	"context"
	"crypto/subtle"
	_ "embed"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"
)

//go:embed web/index.html
var indexHTML []byte

type App struct {
	dataDir string
	token   string
	port    int
	store   *Store
	job     *Job
	life    *Lifecycle
	quit    chan struct{}
	quitOne sync.Once
}

func (a *App) requestQuit() { a.quitOne.Do(func() { close(a.quit) }) }

func (a *App) statePath(id string) string { return filepath.Join(a.dataDir, "states", id+".json") }

// ---------------------------------------------------------------- 生命周期:页面关闭后自动退出

type Lifecycle struct {
	mu        sync.Mutex
	start     time.Time
	lastPing  time.Time
	byeAt     time.Time
	connected bool
}

func newLifecycle() *Lifecycle { return &Lifecycle{start: time.Now()} }

func (l *Lifecycle) Ping() {
	l.mu.Lock()
	l.lastPing, l.connected = time.Now(), true
	l.mu.Unlock()
}

func (l *Lifecycle) Bye() {
	l.mu.Lock()
	l.byeAt = time.Now()
	l.mu.Unlock()
}

// watch 在没有任何页面连接、且没有任务在跑时退出。
func (a *App) watch() {
	last := time.Now()
	t := time.NewTicker(time.Second)
	defer t.Stop()
	for {
		select {
		case <-a.quit:
			return
		case now := <-t.C:
			l := a.life
			l.mu.Lock()
			if now.Sub(last) > 10*time.Second { // 电脑睡眠后醒来:给页面时间重新连上
				l.lastPing = now
			}
			last = now
			gone := false
			switch {
			case !l.connected:
				gone = now.Sub(l.start) > 3*time.Minute
			case !l.byeAt.IsZero() && l.lastPing.Before(l.byeAt) && now.Sub(l.byeAt) > 4*time.Second:
				gone = true
			case now.Sub(l.lastPing) > 3*time.Minute:
				gone = true
			}
			l.mu.Unlock()
			if gone && !a.job.Running() {
				log.Printf("no page connected, exiting")
				a.requestQuit()
				return
			}
		}
	}
}

// ---------------------------------------------------------------- 鉴权

func (a *App) cookieName() string { return "sd_token_" + strconv.Itoa(a.port) }

func (a *App) hostOK(h string) bool {
	return h == fmt.Sprintf("127.0.0.1:%d", a.port) || h == fmt.Sprintf("localhost:%d", a.port)
}

func (a *App) authed(r *http.Request) bool {
	c, err := r.Cookie(a.cookieName())
	return err == nil && subtle.ConstantTimeCompare([]byte(c.Value), []byte(a.token)) == 1
}

// guard:校验 Host(防 DNS rebinding)、Cookie 令牌,以及非 GET 请求必须带自定义头(防 CSRF)。
func (a *App) guard(h http.HandlerFunc, needHeader bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !a.hostOK(r.Host) || !a.authed(r) {
			jsonErr(w, 403, "未授权:请使用程序启动时打开的地址访问")
			return
		}
		if needHeader && r.Method != http.MethodGet && r.Header.Get("X-SD") != "1" {
			jsonErr(w, 403, "缺少请求头")
			return
		}
		h(w, r)
	}
}

func jsonOut(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func jsonErr(w http.ResponseWriter, code int, msg string) {
	jsonOut(w, code, map[string]string{"error": msg})
}

func readBody(r *http.Request, v any) error {
	return json.NewDecoder(http.MaxBytesReader(nil, r.Body, 8<<20)).Decode(v)
}

// ---------------------------------------------------------------- 路由

func (a *App) routes() http.Handler {
	mux := http.NewServeMux()
	api := func(h http.HandlerFunc) http.HandlerFunc { return a.guard(h, true) }

	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, "syncdelta") })
	mux.HandleFunc("GET /{$}", a.hIndex)

	mux.HandleFunc("GET /api/state", api(a.hState))
	mux.HandleFunc("PUT /api/profile", api(a.hProfilePut))
	mux.HandleFunc("DELETE /api/profile", api(a.hProfileDelete))
	mux.HandleFunc("GET /api/baseline", api(a.hBaseline))
	mux.HandleFunc("POST /api/baseline/rollback", api(a.hBaselineRollback))
	mux.HandleFunc("POST /api/baseline/clear", api(a.hBaselineClear))
	mux.HandleFunc("GET /api/tree", api(a.hTree))
	mux.HandleFunc("GET /api/gitignore", api(a.hGitignore))
	mux.HandleFunc("GET /api/fs", api(a.hFs))
	mux.HandleFunc("POST /api/scan", api(a.hScan))
	mux.HandleFunc("POST /api/export", api(a.hExport))
	mux.HandleFunc("GET /api/job", api(a.hJob))
	mux.HandleFunc("POST /api/job/cancel", api(func(w http.ResponseWriter, r *http.Request) {
		a.job.Cancel()
		jsonOut(w, 200, map[string]bool{"ok": true})
	}))
	mux.HandleFunc("GET /api/inc/list", api(a.hIncList))
	mux.HandleFunc("GET /api/inc/preview", api(a.hIncPreview))
	mux.HandleFunc("POST /api/apply", api(a.hApply))
	mux.HandleFunc("POST /api/open", api(a.hOpen))
	mux.HandleFunc("POST /api/ping", api(func(w http.ResponseWriter, r *http.Request) {
		a.life.Ping()
		jsonOut(w, 200, map[string]bool{"ok": true})
	}))
	// sendBeacon 不能带自定义头,所以 bye 只靠 Host + SameSite=Strict 的 Cookie
	mux.HandleFunc("POST /api/bye", a.guard(func(w http.ResponseWriter, r *http.Request) { a.life.Bye(); w.WriteHeader(204) }, false))
	mux.HandleFunc("POST /api/quit", api(func(w http.ResponseWriter, r *http.Request) {
		jsonOut(w, 200, map[string]bool{"ok": true})
		go func() { time.Sleep(200 * time.Millisecond); a.requestQuit() }()
	}))
	return mux
}

func (a *App) hIndex(w http.ResponseWriter, r *http.Request) {
	if !a.hostOK(r.Host) {
		http.Error(w, "forbidden", 403)
		return
	}
	if t := r.URL.Query().Get("t"); t != "" {
		if subtle.ConstantTimeCompare([]byte(t), []byte(a.token)) != 1 {
			http.Error(w, "链接已失效,请重新启动程序", 403)
			return
		}
		http.SetCookie(w, &http.Cookie{Name: a.cookieName(), Value: a.token, Path: "/", HttpOnly: true, SameSite: http.SameSiteStrictMode})
		http.Redirect(w, r, "/", http.StatusFound)
		return
	}
	if !a.authed(r) {
		http.Error(w, "请使用程序启动时打开的地址访问(或重新运行程序)", 403)
		return
	}
	a.life.Ping()
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	_, _ = w.Write(indexHTML)
}

// ---------------------------------------------------------------- 任务配置

func (a *App) hState(w http.ResponseWriter, r *http.Request) {
	inc, tgt := a.store.ApplyDefaults()
	home, _ := os.UserHomeDir()
	jsonOut(w, 200, map[string]any{
		"version": version, "os": runtime.GOOS, "arch": runtime.GOARCH, "home": home,
		"sep": string(filepath.Separator), "profiles": a.store.List(), "lastInc": inc, "lastTarget": tgt,
	})
}

func cleanDir(p string) (string, error) {
	p = strings.TrimSpace(p)
	if p == "" {
		return "", nil
	}
	if !filepath.IsAbs(p) {
		return "", fmt.Errorf("请输入绝对路径: %s", p)
	}
	return filepath.Clean(p), nil
}

func (a *App) hProfilePut(w http.ResponseWriter, r *http.Request) {
	var p Profile
	if err := readBody(r, &p); err != nil {
		jsonErr(w, 400, "请求格式错误")
		return
	}
	p.Name = strings.TrimSpace(p.Name)
	if p.Name == "" {
		p.Name = "未命名任务"
	}
	var err error
	if p.Src, err = cleanDir(p.Src); err != nil {
		jsonErr(w, 400, err.Error())
		return
	}
	if p.Out, err = cleanDir(p.Out); err != nil {
		jsonErr(w, 400, err.Error())
		return
	}
	if p.Src != "" && p.Out != "" && pkey(p.Src) == pkey(p.Out) {
		jsonErr(w, 400, "导出目录不能与源文件夹相同")
		return
	}
	var bad []string
	p.ExcludePatterns, bad = NormalizePatterns(p.ExcludePatterns)
	if len(bad) > 0 {
		jsonErr(w, 400, "排除规则语法错误: "+strings.Join(bad, ", "))
		return
	}
	p.ExcludePaths = NormalizeRelPaths(p.ExcludePaths)
	if p.ExcludePaths == nil {
		p.ExcludePaths = []string{}
	}
	if p.ExcludePatterns == nil {
		p.ExcludePatterns = []string{}
	}
	saved, err := a.store.Upsert(p)
	if err != nil {
		jsonErr(w, 500, "保存失败: "+err.Error())
		return
	}
	jsonOut(w, 200, saved)
}

func (a *App) hProfileDelete(w http.ResponseWriter, r *http.Request) {
	id := r.URL.Query().Get("id")
	if _, ok := a.store.Get(id); !ok {
		jsonErr(w, 404, "任务不存在")
		return
	}
	if err := a.store.Delete(id); err != nil {
		jsonErr(w, 500, err.Error())
		return
	}
	_ = os.Remove(a.statePath(id))
	_ = os.Remove(a.statePath(id) + ".bak")
	jsonOut(w, 200, map[string]bool{"ok": true})
}

func (a *App) profileFromQuery(w http.ResponseWriter, r *http.Request) (Profile, bool) {
	p, ok := a.store.Get(r.URL.Query().Get("id"))
	if !ok {
		jsonErr(w, 404, "任务不存在")
	}
	return p, ok
}

func (a *App) hBaseline(w http.ResponseWriter, r *http.Request) {
	if p, ok := a.profileFromQuery(w, r); ok {
		jsonOut(w, 200, baselineInfo(a.statePath(p.ID)))
	}
}

func (a *App) hBaselineRollback(w http.ResponseWriter, r *http.Request) {
	if p, ok := a.profileFromQuery(w, r); ok {
		if a.job.Running() {
			jsonErr(w, 409, errBusy.Error())
			return
		}
		if err := rollbackState(a.statePath(p.ID)); err != nil {
			jsonErr(w, 400, err.Error())
			return
		}
		jsonOut(w, 200, baselineInfo(a.statePath(p.ID)))
	}
}

func (a *App) hBaselineClear(w http.ResponseWriter, r *http.Request) {
	if p, ok := a.profileFromQuery(w, r); ok {
		if a.job.Running() {
			jsonErr(w, 409, errBusy.Error())
			return
		}
		if err := clearState(a.statePath(p.ID)); err != nil {
			jsonErr(w, 500, err.Error())
			return
		}
		jsonOut(w, 200, baselineInfo(a.statePath(p.ID)))
	}
}

// ---------------------------------------------------------------- 文件系统浏览

func (a *App) hTree(w http.ResponseWriter, r *http.Request) {
	p, ok := a.profileFromQuery(w, r)
	if !ok {
		return
	}
	if p.Src == "" {
		jsonErr(w, 400, "请先选择源文件夹")
		return
	}
	rel := strings.Trim(r.URL.Query().Get("rel"), "/")
	m := NewMatcher(p.ExcludePaths, p.ExcludePatterns)
	ents, trunc, err := listTree(p.Src, rel, m)
	if err != nil {
		jsonErr(w, 400, err.Error())
		return
	}
	jsonOut(w, 200, map[string]any{"entries": ents, "truncated": trunc})
}

// hGitignore 读取源文件夹根目录下的 .gitignore,供界面导入到排除规则里。
func (a *App) hGitignore(w http.ResponseWriter, r *http.Request) {
	p, ok := a.profileFromQuery(w, r)
	if !ok {
		return
	}
	if p.Src == "" {
		jsonErr(w, 400, "请先选择源文件夹")
		return
	}
	f, err := os.Open(filepath.Join(p.Src, ".gitignore"))
	if err != nil {
		jsonErr(w, 404, "源文件夹根目录下没有 .gitignore")
		return
	}
	defer f.Close()
	b, err := io.ReadAll(io.LimitReader(f, 1<<20))
	if err != nil {
		jsonErr(w, 400, err.Error())
		return
	}
	jsonOut(w, 200, map[string]any{"text": string(b)})
}

func (a *App) hFs(w http.ResponseWriter, r *http.Request) {
	l, err := listDir(r.URL.Query().Get("path"), r.URL.Query().Get("files") != "1")
	if err != nil {
		jsonErr(w, 400, err.Error())
		return
	}
	jsonOut(w, 200, l)
}

// ---------------------------------------------------------------- 扫描 / 导出

func (a *App) runParams(p Profile) RunParams {
	return RunParams{P: p, StatePath: a.statePath(p.ID), DataDir: a.dataDir}
}

func (a *App) hScan(w http.ResponseWriter, r *http.Request) {
	p, ok := a.profileFromQuery(w, r)
	if !ok {
		return
	}
	rp := a.runParams(p)
	if err := a.job.Start("scan", func(ctx context.Context, prog Progress) (any, error) {
		return RunScan(ctx, rp, prog)
	}); err != nil {
		jsonErr(w, 409, err.Error())
		return
	}
	jsonOut(w, 202, map[string]bool{"started": true})
}

func (a *App) hExport(w http.ResponseWriter, r *http.Request) {
	p, ok := a.profileFromQuery(w, r)
	if !ok {
		return
	}
	mode := r.URL.Query().Get("mode")
	if mode != "baseline" {
		mode = "export"
	}
	rp := a.runParams(p)
	if err := a.job.Start(mode, func(ctx context.Context, prog Progress) (any, error) {
		res, err := RunExport(ctx, rp, mode, prog)
		if err != nil {
			return nil, err
		}
		if !res.NoChanges {
			a.store.SetLastRun(p.ID, RunInfo{Time: time.Now().Format("2006-01-02 15:04:05"), Mode: mode,
				Added: res.AddedN, Modified: res.ModifiedN, Deleted: res.DeletedN, IncDir: res.IncDir})
		}
		return res, nil
	}); err != nil {
		jsonErr(w, 409, err.Error())
		return
	}
	jsonOut(w, 202, map[string]bool{"started": true})
}

func (a *App) hJob(w http.ResponseWriter, r *http.Request) { jsonOut(w, 200, a.job.View()) }

// ---------------------------------------------------------------- 应用增量

func (a *App) hIncList(w http.ResponseWriter, r *http.Request) {
	dir := r.URL.Query().Get("dir")
	if dir == "" {
		jsonOut(w, 200, map[string]any{"incs": []IncInfo{}})
		return
	}
	incs, err := listIncs(filepath.Clean(dir))
	if err != nil {
		jsonErr(w, 400, err.Error())
		return
	}
	target := r.URL.Query().Get("target")
	for i := range incs {
		if target != "" {
			if t, ok := a.store.IsApplied(target, incs[i].Name); ok {
				incs[i].Applied = t
			}
		}
	}
	if incs == nil {
		incs = []IncInfo{}
	}
	jsonOut(w, 200, map[string]any{"incs": incs})
}

func (a *App) hIncPreview(w http.ResponseWriter, r *http.Request) {
	dir := filepath.Clean(r.URL.Query().Get("dir"))
	if !isIncDir(dir) {
		jsonErr(w, 400, "不是有效的增量包")
		return
	}
	all := readChanges(dir, 0)
	if all == nil {
		all = []Change{}
	}
	changes := all
	if len(changes) > 3000 {
		changes = changes[:3000]
	}
	jsonOut(w, 200, map[string]any{"info": readIncInfo(dir), "changes": changes, "total": len(all)})
}

func (a *App) hApply(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Incs   []string `json:"incs"`
		Target string   `json:"target"`
		Delete bool     `json:"delete"`
	}
	if err := readBody(r, &req); err != nil || len(req.Incs) == 0 {
		jsonErr(w, 400, "请选择要应用的增量包")
		return
	}
	target, err := cleanDir(req.Target)
	if err != nil || target == "" {
		jsonErr(w, 400, "请选择目标文件夹(绝对路径)")
		return
	}
	incs := make([]string, len(req.Incs))
	for i, d := range req.Incs {
		incs[i] = filepath.Clean(d)
	}
	if err := a.job.Start("apply", func(ctx context.Context, prog Progress) (any, error) {
		res, err := ApplyInc(ctx, incs, target, req.Delete, prog, func(inc string) {
			a.store.MarkApplied(target, filepath.Base(inc), time.Now().Format("2006-01-02 15:04:05"))
		})
		if err != nil {
			return res, err
		}
		a.store.SetApplyDefaults(filepath.Dir(incs[0]), target)
		return res, nil
	}); err != nil {
		jsonErr(w, 409, err.Error())
		return
	}
	jsonOut(w, 202, map[string]bool{"started": true})
}

func (a *App) hOpen(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Path string `json:"path"`
	}
	if err := readBody(r, &req); err != nil {
		jsonErr(w, 400, "请求格式错误")
		return
	}
	p := filepath.Clean(req.Path)
	if fi, err := os.Stat(p); err != nil || !fi.IsDir() {
		jsonErr(w, 400, "文件夹不存在")
		return
	}
	if err := revealDir(p); err != nil {
		jsonErr(w, 500, err.Error())
		return
	}
	jsonOut(w, 200, map[string]bool{"ok": true})
}
