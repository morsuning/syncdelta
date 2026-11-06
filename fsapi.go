// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at https://mozilla.org/MPL/2.0/.

package main

import (
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
)

type FsEntry struct {
	Name  string `json:"name"`
	IsDir bool   `json:"isDir"`
	Size  int64  `json:"size"`
	Mtime int64  `json:"mtime"`
}

type Shortcut struct {
	Name string `json:"name"`
	Path string `json:"path"`
}

type FsListing struct {
	Path      string     `json:"path"`   // 当前目录(Windows 驱动器列表时为空)
	Parent    string     `json:"parent"` // 上一级;已在顶层则为空
	Entries   []FsEntry  `json:"entries"`
	Shortcuts []Shortcut `json:"shortcuts"`
	Truncated bool       `json:"truncated"`
}

const maxEntries = 5000

func shortcuts() []Shortcut {
	var out []Shortcut
	add := func(name, p string) {
		if fi, err := os.Stat(p); err == nil && fi.IsDir() {
			out = append(out, Shortcut{name, p})
		}
	}
	if home, err := os.UserHomeDir(); err == nil {
		add("主目录", home)
		add("桌面", filepath.Join(home, "Desktop"))
		add("文档", filepath.Join(home, "Documents"))
		add("下载", filepath.Join(home, "Downloads"))
	}
	addChildren := func(dir, prefix string) {
		ents, err := os.ReadDir(dir)
		if err != nil {
			return
		}
		for _, e := range ents {
			if e.IsDir() && !strings.HasPrefix(e.Name(), ".") {
				add(prefix+e.Name(), filepath.Join(dir, e.Name()))
			}
		}
	}
	switch runtime.GOOS {
	case "windows":
		for c := 'A'; c <= 'Z'; c++ {
			d := string(c) + `:\`
			if _, err := os.Stat(d); err == nil {
				out = append(out, Shortcut{"磁盘 " + string(c) + ":", d})
			}
		}
	case "darwin":
		addChildren("/Volumes", "卷 ")
	default:
		user := os.Getenv("USER")
		addChildren(filepath.Join("/media", user), "介质 ")
		addChildren(filepath.Join("/run/media", user), "介质 ")
		addChildren("/mnt", "挂载 ")
	}
	return out
}

func listDrives() []FsEntry {
	var out []FsEntry
	for c := 'A'; c <= 'Z'; c++ {
		d := string(c) + `:\`
		if _, err := os.Stat(d); err == nil {
			out = append(out, FsEntry{Name: d, IsDir: true})
		}
	}
	return out
}

// listDir 列出目录内容。dirsOnly=true 时只返回子文件夹(用于选择器)。
func listDir(p string, dirsOnly bool) (*FsListing, error) {
	l := &FsListing{Shortcuts: shortcuts()}
	if p == "" {
		if runtime.GOOS == "windows" {
			l.Entries = listDrives()
			return l, nil
		}
		home, err := os.UserHomeDir()
		if err != nil {
			home = "/"
		}
		p = home
	}
	p = filepath.Clean(p)
	ents, err := os.ReadDir(p)
	if err != nil {
		return nil, err
	}
	l.Path = p
	if parent := filepath.Dir(p); parent != p {
		l.Parent = parent
	} else if runtime.GOOS == "windows" {
		l.Parent = "" // 驱动器根目录,界面上"上一级"回到驱动器列表
	}
	for _, e := range ents {
		isDir := e.IsDir()
		if e.Type()&fs.ModeSymlink != 0 {
			if fi, err := os.Stat(filepath.Join(p, e.Name())); err == nil {
				isDir = fi.IsDir()
			}
		}
		if dirsOnly && !isDir {
			continue
		}
		fe := FsEntry{Name: e.Name(), IsDir: isDir}
		if !isDir {
			if fi, err := e.Info(); err == nil {
				fe.Size, fe.Mtime = fi.Size(), fi.ModTime().Unix()
			}
		}
		l.Entries = append(l.Entries, fe)
		if len(l.Entries) >= maxEntries {
			l.Truncated = true
			break
		}
	}
	sort.SliceStable(l.Entries, func(i, j int) bool {
		a, b := l.Entries[i], l.Entries[j]
		if a.IsDir != b.IsDir {
			return a.IsDir
		}
		return strings.ToLower(a.Name) < strings.ToLower(b.Name)
	})
	return l, nil
}

// ---------------------------------------------------------------- 排除树

type TreeEntry struct {
	Name     string `json:"name"`
	Rel      string `json:"rel"`
	IsDir    bool   `json:"isDir"`
	Size     int64  `json:"size"`
	Excluded string `json:"excluded"` // "" / path / pattern / parent
}

func listTree(src, rel string, m *Matcher) ([]TreeEntry, bool, error) {
	dir := src
	if rel != "" {
		var ok bool
		if dir, ok = safeJoin(src, rel); !ok {
			return nil, false, os.ErrInvalid
		}
	}
	l, err := listDir(dir, false)
	if err != nil {
		return nil, false, err
	}
	out := make([]TreeEntry, 0, len(l.Entries))
	for _, e := range l.Entries {
		r := e.Name
		if rel != "" {
			r = rel + "/" + e.Name
		}
		out = append(out, TreeEntry{Name: e.Name, Rel: r, IsDir: e.IsDir, Size: e.Size, Excluded: m.Check(r, e.IsDir)})
	}
	return out, l.Truncated, nil
}
