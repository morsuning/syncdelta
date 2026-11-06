// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at https://mozilla.org/MPL/2.0/.

package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync/atomic"
	"time"
)

const stateVersion = 1

// 基线文件格式与早先的 snapsync.py 完全兼容。
type FileEntry struct {
	Size    int64  `json:"size"`
	MtimeNs int64  `json:"mtime_ns"`
	Sha256  string `json:"sha256"`
}

type State struct {
	Version int                  `json:"version"`
	Src     string               `json:"src"`
	Updated string               `json:"updated"`
	Files   map[string]FileEntry `json:"files"`
}

type Progress func(phase string, done, total int, cur string)

// ---------------------------------------------------------------- 基线文件

func loadState(p string) (*State, error) {
	b, err := os.ReadFile(p)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var st State
	if err := json.Unmarshal(b, &st); err != nil {
		return nil, fmt.Errorf("基线文件已损坏(%s): %w", p, err)
	}
	if st.Version != stateVersion {
		return nil, fmt.Errorf("不支持的基线版本 %d", st.Version)
	}
	if st.Files == nil {
		st.Files = map[string]FileEntry{}
	}
	return &st, nil
}

func writeFileAtomic(p string, b []byte) error {
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return err
	}
	tmp := p + ".tmp"
	if err := os.WriteFile(tmp, b, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, p)
}

// saveState 保存新基线,并把上一份留作 .bak。
func saveState(p string, st *State) error {
	st.Version = stateVersion
	st.Updated = time.Now().Format("2006-01-02 15:04:05")
	b, err := json.Marshal(st)
	if err != nil {
		return err
	}
	if old, err := os.ReadFile(p); err == nil {
		if err := writeFileAtomic(p+".bak", old); err != nil {
			return err
		}
	}
	return writeFileAtomic(p, b)
}

// rollbackState 在当前基线和上一份基线(.bak)之间互换,所以可以反复"撤销/重做"。
func rollbackState(p string) error {
	bak, err := os.ReadFile(p + ".bak")
	if err != nil {
		return errors.New("没有可回退的上一份基线")
	}
	cur, curErr := os.ReadFile(p)
	if err := writeFileAtomic(p, bak); err != nil {
		return err
	}
	if curErr == nil {
		return writeFileAtomic(p+".bak", cur)
	}
	return os.Remove(p + ".bak")
}

// clearState 清除基线(当前基线移到 .bak,仍可通过回退找回)。
func clearState(p string) error {
	cur, err := os.ReadFile(p)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if err := writeFileAtomic(p+".bak", cur); err != nil {
		return err
	}
	return os.Remove(p)
}

type BaselineInfo struct {
	Exists  bool   `json:"exists"`
	HasBak  bool   `json:"hasBak"`
	Updated string `json:"updated"`
	Files   int    `json:"files"`
	Src     string `json:"src"`
	Error   string `json:"error,omitempty"`
}

func baselineInfo(p string) BaselineInfo {
	var bi BaselineInfo
	if _, err := os.Stat(p + ".bak"); err == nil {
		bi.HasBak = true
	}
	st, err := loadState(p)
	if err != nil {
		bi.Error = err.Error()
		return bi
	}
	if st != nil {
		bi.Exists, bi.Updated, bi.Files, bi.Src = true, st.Updated, len(st.Files), st.Src
	}
	return bi
}

// ---------------------------------------------------------------- 哈希 / 复制

func hashFile(ctx context.Context, p string) (string, error) {
	f, err := os.Open(p)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	buf := make([]byte, 1<<20)
	for {
		if err := ctx.Err(); err != nil {
			return "", err
		}
		n, err := f.Read(buf)
		if n > 0 {
			h.Write(buf[:n])
		}
		if err == io.EOF {
			break
		}
		if err != nil {
			return "", err
		}
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// copyFile 先写临时文件再改名,避免出现写了一半的文件;同时算出实际复制内容的哈希。
func copyFile(ctx context.Context, src, dst string) (int64, string, error) {
	in, err := os.Open(src)
	if err != nil {
		return 0, "", err
	}
	defer in.Close()
	info, err := in.Stat()
	if err != nil {
		return 0, "", err
	}
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return 0, "", err
	}
	tmp := dst + ".sdtmp"
	out, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, info.Mode().Perm()|0o600)
	if err != nil {
		return 0, "", err
	}
	cleanup := func() { out.Close(); os.Remove(tmp) }
	h := sha256.New()
	buf := make([]byte, 1<<20)
	var total int64
	for {
		if err := ctx.Err(); err != nil {
			cleanup()
			return 0, "", err
		}
		n, rerr := in.Read(buf)
		if n > 0 {
			if _, werr := out.Write(buf[:n]); werr != nil {
				cleanup()
				return 0, "", werr
			}
			h.Write(buf[:n])
			total += int64(n)
		}
		if rerr == io.EOF {
			break
		}
		if rerr != nil {
			cleanup()
			return 0, "", rerr
		}
	}
	if err := out.Close(); err != nil {
		os.Remove(tmp)
		return 0, "", err
	}
	_ = os.Chtimes(tmp, info.ModTime(), info.ModTime())
	if err := os.Rename(tmp, dst); err != nil {
		// Windows 上目标是只读文件时 Rename 会失败,删掉再试一次
		_ = os.Remove(dst)
		if err2 := os.Rename(tmp, dst); err2 != nil {
			os.Remove(tmp)
			return 0, "", err2
		}
	}
	return total, hex.EncodeToString(h.Sum(nil)), nil
}

// ---------------------------------------------------------------- 扫描

type ScanOptions struct {
	Root     string
	Old      map[string]FileEntry
	M        *Matcher
	Rehash   bool
	Skip     map[string]bool
	Progress Progress
}

type ScanResult struct {
	Files    map[string]FileEntry
	Excluded int
	Warnings []string
}

func scanTree(ctx context.Context, o ScanOptions) (*ScanResult, error) {
	res := &ScanResult{Files: map[string]FileEntry{}}
	total := len(o.Old)
	done := 0
	warn := func(format string, a ...any) {
		if len(res.Warnings) < 50 {
			res.Warnings = append(res.Warnings, fmt.Sprintf(format, a...))
		}
	}
	err := filepath.WalkDir(o.Root, func(p string, d fs.DirEntry, err error) error {
		if cerr := ctx.Err(); cerr != nil {
			return cerr
		}
		if err != nil {
			warn("无法读取 %s: %v", p, err)
			if d != nil && d.IsDir() {
				return fs.SkipDir
			}
			return nil
		}
		if p == o.Root {
			return nil
		}
		if o.Skip[pkey(p)] {
			if d.IsDir() {
				return fs.SkipDir
			}
			return nil
		}
		relOS, rerr := filepath.Rel(o.Root, p)
		if rerr != nil {
			return nil
		}
		rel := filepath.ToSlash(relOS)
		if o.M.Match(rel, d.IsDir()) != "" {
			res.Excluded++
			if d.IsDir() {
				return fs.SkipDir
			}
			return nil
		}
		if d.IsDir() {
			return nil
		}
		var info fs.FileInfo
		if d.Type()&fs.ModeSymlink != 0 {
			info, err = os.Stat(p) // 符号链接指向文件时按文件处理;指向目录则跳过(不跟随)
		} else {
			info, err = d.Info()
		}
		if err != nil {
			warn("无法读取 %s: %v", rel, err)
			return nil
		}
		if !info.Mode().IsRegular() {
			return nil
		}
		e := FileEntry{Size: info.Size(), MtimeNs: info.ModTime().UnixNano()}
		if old, ok := o.Old[rel]; ok && !o.Rehash && old.Size == e.Size && old.MtimeNs == e.MtimeNs {
			e.Sha256 = old.Sha256
		} else {
			sum, herr := hashFile(ctx, p)
			if herr != nil {
				if errors.Is(herr, context.Canceled) {
					return herr
				}
				warn("无法读取 %s: %v", rel, herr)
				return nil
			}
			e.Sha256 = sum
		}
		res.Files[rel] = e
		done++
		if o.Progress != nil {
			o.Progress("扫描", done, total, rel)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return res, nil
}

// ---------------------------------------------------------------- 差异

type ChangeItem struct {
	Path string `json:"path"`
	Size int64  `json:"size"`
}

type DiffResult struct {
	Added     []ChangeItem
	Modified  []ChangeItem
	Deleted   []string
	Unchanged int
	Dropped   int // 基线里有、但现在被新的排除规则排除的文件(不算删除,目标端不会动它们)
}

func computeDiff(old, cur map[string]FileEntry, m *Matcher) *DiffResult {
	d := &DiffResult{}
	for rel, e := range cur {
		o, ok := old[rel]
		switch {
		case !ok:
			d.Added = append(d.Added, ChangeItem{rel, e.Size})
		case o.Sha256 != e.Sha256:
			d.Modified = append(d.Modified, ChangeItem{rel, e.Size})
		default:
			d.Unchanged++
		}
	}
	for rel := range old {
		if _, ok := cur[rel]; ok {
			continue
		}
		if m.Check(rel, false) != "" {
			d.Dropped++
		} else {
			d.Deleted = append(d.Deleted, rel)
		}
	}
	sort.Slice(d.Added, func(i, j int) bool { return d.Added[i].Path < d.Added[j].Path })
	sort.Slice(d.Modified, func(i, j int) bool { return d.Modified[i].Path < d.Modified[j].Path })
	sort.Strings(d.Deleted)
	return d
}

// ---------------------------------------------------------------- 导出

type RunParams struct {
	P         Profile
	StatePath string
	DataDir   string
}

type ExportResult struct {
	Mode      string       `json:"mode"` // scan / export / baseline
	NoChanges bool         `json:"noChanges"`
	HadBase   bool         `json:"hadBaseline"`
	Added     []ChangeItem `json:"added"`
	Modified  []ChangeItem `json:"modified"`
	Deleted   []string     `json:"deleted"`
	AddedN    int          `json:"addedN"`
	ModifiedN int          `json:"modifiedN"`
	DeletedN  int          `json:"deletedN"`
	Total     int          `json:"total"`
	Unchanged int          `json:"unchanged"`
	Excluded  int          `json:"excluded"`
	Dropped   int          `json:"dropped"`
	Bytes     int64        `json:"bytes"`
	PackBytes int64        `json:"packBytes"`
	Level     string       `json:"level"`
	IncDir    string       `json:"incDir"`
	Warnings  []string     `json:"warnings"`
	Truncated bool         `json:"truncated"`
}

const listCap = 20000

type plan struct {
	st   *State
	scan *ScanResult
	diff *DiffResult
	root string
	warn []string
}

func prepare(ctx context.Context, rp RunParams, prog Progress) (*plan, error) {
	p := rp.P
	if p.Src == "" {
		return nil, errors.New("请先选择源文件夹")
	}
	root := filepath.Clean(p.Src)
	if fi, err := os.Stat(root); err != nil || !fi.IsDir() {
		return nil, fmt.Errorf("源文件夹不存在或不是文件夹: %s", root)
	}
	if p.Out != "" && pkey(p.Out) == pkey(root) {
		return nil, errors.New("导出目录不能与源文件夹相同")
	}
	st, err := loadState(rp.StatePath)
	if err != nil {
		return nil, err
	}
	var old map[string]FileEntry
	pl := &plan{root: root}
	if st != nil {
		old = st.Files
		if st.Src != "" && pkey(st.Src) != pkey(root) {
			pl.warn = append(pl.warn, fmt.Sprintf("基线是针对另一个文件夹记录的(%s)。如果现在这个是不同的文件夹,请先在「基线」里清除基线。", st.Src))
		}
	}
	skip := map[string]bool{}
	for _, s := range []string{p.Out, rp.StatePath, rp.StatePath + ".bak", rp.StatePath + ".tmp", rp.DataDir} {
		if s != "" {
			skip[pkey(s)] = true
		}
	}
	m := NewMatcher(p.ExcludePaths, p.ExcludePatterns)
	sr, err := scanTree(ctx, ScanOptions{Root: root, Old: old, M: m, Rehash: p.Rehash, Skip: skip, Progress: prog})
	if err != nil {
		return nil, err
	}
	pl.st, pl.scan, pl.diff = st, sr, computeDiff(old, sr.Files, m)
	return pl, nil
}

func (pl *plan) summarize(mode string) *ExportResult {
	d := pl.diff
	r := &ExportResult{
		Mode: mode, HadBase: pl.st != nil,
		AddedN: len(d.Added), ModifiedN: len(d.Modified), DeletedN: len(d.Deleted),
		Total: len(pl.scan.Files), Unchanged: d.Unchanged, Excluded: pl.scan.Excluded, Dropped: d.Dropped,
		Warnings: append(append([]string{}, pl.warn...), pl.scan.Warnings...),
	}
	capItems := func(in []ChangeItem) []ChangeItem {
		if len(in) > listCap {
			r.Truncated = true
			return in[:listCap]
		}
		return in
	}
	r.Added, r.Modified = capItems(d.Added), capItems(d.Modified)
	r.Deleted = d.Deleted
	if len(r.Deleted) > listCap {
		r.Truncated = true
		r.Deleted = r.Deleted[:listCap]
	}
	r.nonNil()
	return r
}

// 空切片要序列化成 [] 而不是 null,否则前端遍历会出错。
func (r *ExportResult) nonNil() {
	if r.Added == nil {
		r.Added = []ChangeItem{}
	}
	if r.Modified == nil {
		r.Modified = []ChangeItem{}
	}
	if r.Deleted == nil {
		r.Deleted = []string{}
	}
	if r.Warnings == nil {
		r.Warnings = []string{}
	}
}

// RunScan 只扫描并给出差异,不写任何东西(基线也不动)。
func RunScan(ctx context.Context, rp RunParams, prog Progress) (*ExportResult, error) {
	pl, err := prepare(ctx, rp, prog)
	if err != nil {
		return nil, err
	}
	return pl.summarize("scan"), nil
}

func uniqueDir(base string) string {
	p := base
	for i := 1; ; i++ {
		if _, err := os.Stat(p); errors.Is(err, fs.ErrNotExist) {
			return p
		}
		p = fmt.Sprintf("%s_%d", base, i)
	}
}

type IncMeta struct {
	Version  int    `json:"version"`
	Profile  string `json:"profile"`
	Source   string `json:"source"`
	Created  string `json:"created"`
	Added    int    `json:"added"`
	Modified int    `json:"modified"`
	Deleted  int    `json:"deleted"`
	Bytes    int64  `json:"bytes"`
	Pack     int64  `json:"pack,omitempty"`   // data.sdpk 的实际大小
	Format   string `json:"format,omitempty"` // sdpk1 / dir
	Level    string `json:"level,omitempty"`
}

// RunExport:mode 为 "export"(导出增量并更新基线)或 "baseline"(只记录基线)。
func RunExport(ctx context.Context, rp RunParams, mode string, prog Progress) (res *ExportResult, err error) {
	pl, err := prepare(ctx, rp, prog)
	if err != nil {
		return nil, err
	}
	newState := &State{Src: pl.root, Files: pl.scan.Files}
	res = pl.summarize(mode)

	if mode == "baseline" {
		if err := saveState(rp.StatePath, newState); err != nil {
			return nil, err
		}
		return res, nil
	}
	d := pl.diff
	if len(d.Added)+len(d.Modified)+len(d.Deleted) == 0 {
		res.NoChanges = true
		if err := saveState(rp.StatePath, newState); err != nil { // 顺便刷新 mtime 缓存
			return nil, err
		}
		return res, nil
	}
	if rp.P.Out == "" {
		return nil, errors.New("请先选择导出目录")
	}
	out, err := filepath.Abs(rp.P.Out)
	if err != nil {
		return nil, err
	}
	incDir := uniqueDir(filepath.Join(out, "inc_"+time.Now().Format("20060102_150405")))
	level, levelName := parseLevel(rp.P.Compress)
	if level == levelCopy {
		if err := os.MkdirAll(filepath.Join(incDir, "files"), 0o755); err != nil {
			return nil, err
		}
	} else if err := os.MkdirAll(incDir, 0o755); err != nil {
		return nil, err
	}
	ok := false
	defer func() {
		if !ok { // 失败/取消时不留下半成品增量包
			os.RemoveAll(incDir)
		}
	}()

	list := append(append([]ChangeItem{}, d.Added...), d.Modified...)
	deleted := append([]string{}, d.Deleted...)
	isNew := map[string]bool{}
	for _, it := range d.Added {
		isNew[it.Path] = true
	}
	var bytes, packSize int64
	nCopied := map[string]bool{}
	gone := func(it ChangeItem) {
		delete(newState.Files, it.Path)
		if !isNew[it.Path] {
			deleted = append(deleted, it.Path)
		}
		res.Warnings = append(res.Warnings, "文件在导出过程中被删除: "+it.Path)
	}
	if level == levelCopy {
		filesDir := filepath.Join(incDir, "files")
		for i, it := range list {
			if prog != nil {
				prog("复制", i, len(list), it.Path)
			}
			sz, sum, err := copyFile(ctx, filepath.Join(pl.root, filepath.FromSlash(it.Path)), filepath.Join(filesDir, filepath.FromSlash(it.Path)))
			if err != nil {
				if errors.Is(err, fs.ErrNotExist) { // 扫描之后、复制之前文件被删了
					gone(it)
					continue
				}
				return nil, fmt.Errorf("复制 %s 失败: %w", it.Path, err)
			}
			e := newState.Files[it.Path]
			e.Size, e.Sha256 = sz, sum // 基线记录"实际复制出去的内容"
			newState.Files[it.Path] = e
			bytes += sz
			nCopied[it.Path] = true
		}
	} else {
		var err error
		bytes, packSize, err = exportPack(ctx, pl.root, list, newState, nCopied, gone, filepath.Join(incDir, packFile), level, prog)
		if err != nil {
			return nil, err
		}
	}
	sort.Strings(deleted)

	var cb, db strings.Builder
	for _, it := range d.Added {
		if nCopied[it.Path] {
			fmt.Fprintf(&cb, "A\t%s\n", it.Path)
		}
	}
	for _, it := range d.Modified {
		if nCopied[it.Path] {
			fmt.Fprintf(&cb, "M\t%s\n", it.Path)
		}
	}
	for _, p := range deleted {
		fmt.Fprintf(&cb, "D\t%s\n", p)
		fmt.Fprintf(&db, "%s\n", p)
	}
	if err := os.WriteFile(filepath.Join(incDir, "CHANGES.txt"), []byte(cb.String()), 0o644); err != nil {
		return nil, err
	}
	if err := os.WriteFile(filepath.Join(incDir, "DELETED.txt"), []byte(db.String()), 0o644); err != nil {
		return nil, err
	}
	nA, nM := 0, 0
	for _, it := range d.Added {
		if nCopied[it.Path] {
			nA++
		}
	}
	for _, it := range d.Modified {
		if nCopied[it.Path] {
			nM++
		}
	}
	meta := IncMeta{Version: 1, Profile: rp.P.Name, Source: pl.root, Created: time.Now().Format("2006-01-02 15:04:05"),
		Added: nA, Modified: nM, Deleted: len(deleted), Bytes: bytes, Pack: packSize, Level: levelName, Format: "dir"}
	if level != levelCopy {
		meta.Format = "sdpk1"
	}
	mb, _ := json.MarshalIndent(meta, "", "  ")
	if err := os.WriteFile(filepath.Join(incDir, "META.json"), mb, 0o644); err != nil {
		return nil, err
	}
	if err := saveState(rp.StatePath, newState); err != nil {
		return nil, err
	}
	ok = true
	res.IncDir, res.Bytes, res.PackBytes, res.Level = incDir, bytes, packSize, levelName
	res.AddedN, res.ModifiedN, res.DeletedN = nA, nM, len(deleted)
	res.Deleted = deleted
	if len(res.Deleted) > listCap {
		res.Deleted = res.Deleted[:listCap]
	}
	res.Total = len(newState.Files)
	res.nonNil()
	return res, nil
}

// ---------------------------------------------------------------- 应用增量(目标端)

type IncInfo struct {
	Dir      string   `json:"dir"`
	Name     string   `json:"name"`
	Meta     *IncMeta `json:"meta,omitempty"`
	Added    int      `json:"added"`
	Modified int      `json:"modified"`
	Deleted  int      `json:"deleted"`
	Applied  string   `json:"applied,omitempty"`
}

func isIncDir(dir string) bool {
	if fi, err := os.Stat(filepath.Join(dir, packFile)); err == nil && fi.Mode().IsRegular() {
		return true
	}
	fi, err := os.Stat(filepath.Join(dir, "files"))
	return err == nil && fi.IsDir()
}

func readIncInfo(dir string) IncInfo {
	ii := IncInfo{Dir: dir, Name: filepath.Base(dir)}
	if b, err := os.ReadFile(filepath.Join(dir, "META.json")); err == nil {
		var m IncMeta
		if json.Unmarshal(b, &m) == nil {
			ii.Meta = &m
			ii.Added, ii.Modified, ii.Deleted = m.Added, m.Modified, m.Deleted
			return ii
		}
	}
	for _, c := range readChanges(dir, 0) {
		switch c.Tag {
		case "A":
			ii.Added++
		case "M":
			ii.Modified++
		case "D":
			ii.Deleted++
		}
	}
	return ii
}

// listIncs:dir 本身是增量包就返回它;否则返回其下所有 inc_* 子目录(按时间升序)。
func listIncs(dir string) ([]IncInfo, error) {
	if isIncDir(dir) {
		return []IncInfo{readIncInfo(dir)}, nil
	}
	ents, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	var out []IncInfo
	for _, e := range ents {
		if e.IsDir() && isIncDir(filepath.Join(dir, e.Name())) {
			out = append(out, readIncInfo(filepath.Join(dir, e.Name())))
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

type Change struct {
	Tag  string `json:"tag"`
	Path string `json:"path"`
}

func readChanges(dir string, limit int) []Change {
	b, err := os.ReadFile(filepath.Join(dir, "CHANGES.txt"))
	if err != nil {
		return nil
	}
	var out []Change
	for _, line := range strings.Split(string(b), "\n") {
		line = strings.TrimRight(line, "\r")
		if i := strings.IndexByte(line, '\t'); i == 1 {
			out = append(out, Change{Tag: line[:1], Path: line[2:]})
			if limit > 0 && len(out) >= limit {
				break
			}
		}
	}
	return out
}

// safeJoin 把相对路径拼到 base 下,拒绝绝对路径和越界("..")。
func safeJoin(base, rel string) (string, bool) {
	rel = filepath.FromSlash(strings.TrimSpace(rel))
	if rel == "" || filepath.IsAbs(rel) || filepath.VolumeName(rel) != "" {
		return "", false
	}
	p := filepath.Join(base, rel)
	r, err := filepath.Rel(base, p)
	if err != nil || r == "." || r == ".." || strings.HasPrefix(r, ".."+string(filepath.Separator)) {
		return "", false
	}
	return p, true
}

type ApplyItem struct {
	Name    string   `json:"name"`
	Copied  int      `json:"copied"`
	Deleted int      `json:"deleted"`
	Skipped []string `json:"skipped,omitempty"`
}

type ApplyResult struct {
	Target string      `json:"target"`
	Items  []ApplyItem `json:"items"`
}

func ApplyInc(ctx context.Context, incDirs []string, target string, doDelete bool, prog Progress, onDone func(inc string)) (*ApplyResult, error) {
	target = filepath.Clean(target)
	if target == "" || target == "." {
		return nil, errors.New("请先选择目标文件夹")
	}
	for _, d := range incDirs {
		if !isIncDir(d) {
			return nil, fmt.Errorf("不是有效的增量包(缺少 %s 或 files 目录): %s", packFile, d)
		}
		if ii := readIncInfo(d); ii.Meta != nil && pkey(ii.Meta.Source) == pkey(target) {
			return nil, fmt.Errorf("目标文件夹不能是增量包的来源文件夹: %s", target)
		}
	}
	if err := os.MkdirAll(target, 0o755); err != nil {
		return nil, err
	}
	sort.Slice(incDirs, func(i, j int) bool { return filepath.Base(incDirs[i]) < filepath.Base(incDirs[j]) })

	res := &ApplyResult{Target: target}
	for n, inc := range incDirs {
		item := ApplyItem{Name: filepath.Base(inc)}
		phase := fmt.Sprintf("应用 %s (%d/%d)", item.Name, n+1, len(incDirs))
		if _, err := os.Stat(filepath.Join(inc, packFile)); err == nil {
			pr, err := openPack(filepath.Join(inc, packFile))
			if err != nil {
				return res, fmt.Errorf("%s: %w", item.Name, err)
			}
			copied, err := pr.extract(ctx, target, prog, phase)
			pr.Close()
			item.Copied = copied
			if err != nil {
				return res, fmt.Errorf("%s: %w", item.Name, err)
			}
		} else {
			filesDir := filepath.Join(inc, "files")
			var files []string
			_ = filepath.WalkDir(filesDir, func(p string, d fs.DirEntry, err error) error {
				if err == nil && d.Type().IsRegular() {
					files = append(files, p)
				}
				return nil
			})
			for i, f := range files {
				rel, _ := filepath.Rel(filesDir, f)
				if prog != nil {
					prog(phase, i, len(files), filepath.ToSlash(rel))
				}
				if _, _, err := copyFile(ctx, f, filepath.Join(target, rel)); err != nil {
					return res, fmt.Errorf("复制 %s 失败: %w", filepath.ToSlash(rel), err)
				}
				item.Copied++
			}
		}
		if doDelete {
			if b, err := os.ReadFile(filepath.Join(inc, "DELETED.txt")); err == nil {
				for _, line := range strings.Split(string(b), "\n") {
					rel := strings.TrimRight(line, "\r")
					if rel == "" {
						continue
					}
					p, ok := safeJoin(target, rel)
					if !ok {
						item.Skipped = append(item.Skipped, rel)
						continue
					}
					if fi, err := os.Lstat(p); err == nil && !fi.IsDir() {
						if err := os.Remove(p); err != nil {
							item.Skipped = append(item.Skipped, rel)
							continue
						}
						item.Deleted++
						for dir := filepath.Dir(p); pkey(dir) != pkey(target); dir = filepath.Dir(dir) {
							if err := os.Remove(dir); err != nil { // 目录非空会失败,正好停止
								break
							}
						}
					}
				}
			}
		}
		res.Items = append(res.Items, item)
		if onDone != nil {
			onDone(inc)
		}
	}
	return res, nil
}

// ---------------------------------------------------------------- 压缩导出

const levelCopy = -2 // 不压缩,直接复制成 files/ 目录

func parseLevel(c string) (int, string) {
	switch normalizeCompress(c) {
	case compressCtxmix:
		return LevelCtxmix, compressCtxmix
	case compressNone:
		return levelCopy, compressNone
	}
	return LevelZPAQ, compressZPAQ
}

type packItem struct {
	it   ChangeItem
	abs  string
	ext  string
	comp bool
}

// exportPack 把 list 里的文件读出来,按"扩展名→路径"排序后做成一条固实数据流写入 data.sdpk。
// 基线记录的是实际读到的内容(大小+SHA-256)。
func exportPack(ctx context.Context, root string, list []ChangeItem, st *State, copied map[string]bool,
	gone func(ChangeItem), dst string, level int, prog Progress) (raw, packSize int64, err error) {

	items := make([]packItem, 0, len(list))
	var total int64
	for _, it := range list {
		abs := filepath.Join(root, filepath.FromSlash(it.Path))
		ext := strings.ToLower(filepath.Ext(it.Path))
		pi := packItem{it: it, abs: abs, ext: ext, comp: packCompressible(abs, ext, it.Size)}
		items = append(items, pi)
		total += it.Size
	}
	sort.SliceStable(items, func(i, j int) bool {
		a, b := items[i], items[j]
		if a.comp != b.comp {
			return a.comp // 可压缩的在前
		}
		if a.ext != b.ext {
			return a.ext < b.ext
		}
		return a.it.Path < b.it.Path
	})

	workers := packWorkers()
	var done int64
	cancel := func() bool { return ctx.Err() != nil }
	tick := func(n int) {
		d := atomic.AddInt64(&done, int64(n))
		if prog != nil {
			prog("压缩(MiB)", int(min64(d, total)>>20), int(total>>20)+1, "")
		}
	}
	blockSz := packChooseBlock(total, workers)
	if level == LevelZPAQ { // 标准 ZPAQ 5 级每块要几百 MB 内存:限制块大小和并行数
		blockSz = min(blockSz, 16<<20)
		workers = min(workers, 4)
	}
	w, err := newPackWriter(dst, level, blockSz, workers, cancel, tick)
	if err != nil {
		return 0, 0, err
	}
	ok := false
	defer func() {
		if !ok {
			w.Abort()
		}
	}()

	entries := make([]packEntry, 0, len(items))
	buf := make([]byte, 1<<20)
	for i, pi := range items {
		if prog != nil {
			prog("读取", i, len(items), pi.it.Path)
		}
		f, err := os.Open(pi.abs)
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				gone(pi.it)
				continue
			}
			return 0, 0, fmt.Errorf("读取 %s 失败: %w", pi.it.Path, err)
		}
		fi, err := f.Stat()
		if err != nil {
			f.Close()
			return 0, 0, err
		}
		h := sha256.New()
		var n int64
		for {
			if err := ctx.Err(); err != nil {
				f.Close()
				return 0, 0, err
			}
			k, rerr := f.Read(buf)
			if k > 0 {
				h.Write(buf[:k])
				if err := w.AddRaw(buf[:k], pi.comp); err != nil {
					f.Close()
					return 0, 0, err
				}
				n += int64(k)
			}
			if rerr == io.EOF {
				break
			}
			if rerr != nil {
				f.Close()
				return 0, 0, fmt.Errorf("读取 %s 失败: %w", pi.it.Path, rerr)
			}
		}
		f.Close()
		e := st.Files[pi.it.Path]
		e.Size, e.Sha256 = n, hex.EncodeToString(h.Sum(nil))
		st.Files[pi.it.Path] = e
		entries = append(entries, packEntry{Path: pi.it.Path, Size: n, Perm: uint32(fi.Mode().Perm()), Mtime: fi.ModTime().Unix()})
		raw += n
		copied[pi.it.Path] = true
	}
	if prog != nil {
		prog("压缩(MiB)", 0, int(raw>>20)+1, "")
	}
	packSize, err = w.Close(entries)
	if err != nil {
		return 0, 0, err
	}
	ok = true
	return raw, packSize, nil
}
