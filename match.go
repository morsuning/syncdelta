// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at https://mozilla.org/MPL/2.0/.

package main

import (
	"path"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
)

// Windows / macOS 的文件系统默认不区分大小写,这里统一折叠大小写来比较。
func foldCase() bool { return runtime.GOOS == "windows" || runtime.GOOS == "darwin" }

func fkey(s string) string {
	if foldCase() {
		return strings.ToLower(s)
	}
	return s
}

// pkey 把一个本地路径规范化成可比较的 key。
func pkey(p string) string { return fkey(filepath.Clean(p)) }

// ---------------------------------------------------------------- 排除规则(.gitignore 语义)
//
// 规则语法与 .gitignore 一致(参考 git 文档 gitignore(5)):
//   - 空行忽略;# 开头是注释(\# 表示字面的 #);行尾未转义的空格被忽略(\ 表示字面空格)
//   - ! 开头表示取消排除(重新包含);\! 表示字面的 !
//   - 末尾的 / 只匹配文件夹
//   - 开头或中间含 / 的规则相对源文件夹根匹配,否则在任意层级匹配名称
//   - * 匹配除 / 外的任意字符,? 匹配除 / 外的一个字符,[a-z] [!a-z] [^a-z] 字符集,\ 转义
//   - 前导 **/ 匹配所有层级;末尾 /** 匹配内部所有内容;中间 /**/ 匹配零或多层文件夹
//   - 同一路径命中多条规则时,最后一条生效
//   - 上级文件夹被排除后,里面的东西无法再被 ! 重新包含(与 git 相同)
// Windows / macOS 上不区分大小写。

type gseg struct {
	s     string // 已折叠大小写的原文
	r     []rune
	lit   bool // 没有通配符,直接比较字符串
	dstar bool // 恰好是 "**"
}

type pattern struct {
	raw      string
	neg      bool
	dirOnly  bool
	anchored bool
	segs     []gseg
}

// Matcher 由两部分组成:
//   - 精确路径(用户在目录树里勾选的文件/文件夹,相对源目录,用 / 分隔),始终排除
//   - 规则(每行一条,.gitignore 语法)
type Matcher struct {
	paths    map[string]bool
	patterns []pattern
}

func trimUnescapedSpaces(s string) string {
	s = strings.TrimRight(s, "\r\n")
	for len(s) > 0 && s[len(s)-1] == ' ' {
		nb := 0
		for i := len(s) - 2; i >= 0 && s[i] == '\\'; i-- {
			nb++
		}
		if nb%2 == 1 {
			break
		}
		s = s[:len(s)-1]
	}
	return s
}

// globClass 解析 p[i]=='[' 开始的字符集,返回是否匹配 c、下一个位置、是否是合法字符集(有结尾的 ])。
func globClass(p []rune, i int, c rune) (matched bool, next int, ok bool) {
	j := i + 1
	neg := false
	if j < len(p) && (p[j] == '!' || p[j] == '^') {
		neg = true
		j++
	}
	first := true
	for j < len(p) {
		if p[j] == ']' && !first {
			return matched != neg, j + 1, true
		}
		first = false
		lo := p[j]
		if lo == '\\' && j+1 < len(p) {
			j++
			lo = p[j]
		}
		j++
		hi := lo
		if j+1 < len(p) && p[j] == '-' && p[j+1] != ']' {
			hi = p[j+1]
			j++
			if hi == '\\' && j+1 < len(p) {
				j++
				hi = p[j]
			}
			j++
		}
		if lo <= c && c <= hi {
			matched = true
		}
	}
	return false, 0, false
}

// globValid 检查单段通配符是否语法完整(没有悬空的 \ 或未闭合的 [)。
func globValid(r []rune) bool {
	for i := 0; i < len(r); i++ {
		switch r[i] {
		case '\\':
			if i+1 >= len(r) {
				return false
			}
			i++
		case '[':
			if _, nx, ok := globClass(r, i, 0); !ok {
				return false
			} else {
				i = nx - 1
			}
		}
	}
	return true
}

// globMatch 在单个路径段内做通配符匹配(* ? [..] \x)。
func globMatch(p, s []rune) bool {
	pi, si := 0, 0
	starP, starS := -1, 0
	for si < len(s) {
		if pi < len(p) {
			switch p[pi] {
			case '*':
				for pi < len(p) && p[pi] == '*' {
					pi++
				}
				if pi == len(p) {
					return true
				}
				starP, starS = pi, si
				continue
			case '?':
				pi++
				si++
				continue
			case '[':
				if m, nx, ok := globClass(p, pi, s[si]); ok {
					if m {
						pi, si = nx, si+1
						continue
					}
					goto mismatch
				}
				if s[si] == '[' { // 没有结尾的 ] 时按字面处理
					pi++
					si++
					continue
				}
			case '\\':
				if pi+1 < len(p) && p[pi+1] == s[si] {
					pi += 2
					si++
					continue
				}
			default:
				if p[pi] == s[si] {
					pi++
					si++
					continue
				}
			}
		}
	mismatch:
		if starP >= 0 {
			starS++
			si, pi = starS, starP
			continue
		}
		return false
	}
	for pi < len(p) && p[pi] == '*' {
		pi++
	}
	return pi == len(p)
}

func (g gseg) match(name string) bool {
	if g.lit {
		return g.s == name
	}
	return globMatch(g.r, []rune(name))
}

// matchSegs 把规则段 pat 与路径段 parts 对齐,处理 **。
func matchSegs(pat []gseg, parts []string) bool {
	for len(pat) > 0 {
		g := pat[0]
		if g.dstar {
			if len(pat) == 1 { // 末尾 /**:匹配里面的所有内容(至少一层)
				return len(parts) >= 1
			}
			for k := 0; k <= len(parts); k++ {
				if matchSegs(pat[1:], parts[k:]) {
					return true
				}
			}
			return false
		}
		if len(parts) == 0 || !g.match(parts[0]) {
			return false
		}
		pat, parts = pat[1:], parts[1:]
	}
	return len(parts) == 0
}

func (p *pattern) matches(parts []string) bool {
	if !p.anchored {
		return p.segs[0].match(parts[len(parts)-1])
	}
	return matchSegs(p.segs, parts)
}

// parsePattern 解析一行规则。ok=false 表示这行是空行/注释;err 表示语法有问题。
func parsePattern(line string) (p pattern, ok bool, err error) {
	line = trimUnescapedSpaces(line)
	if line == "" || line[0] == '#' {
		return pattern{}, false, nil
	}
	p.raw = line
	if line[0] == '!' {
		p.neg = true
		line = line[1:]
	}
	if strings.HasSuffix(line, "/") {
		p.dirOnly = true
		line = strings.TrimRight(line, "/")
	}
	if strings.Contains(line, "/") {
		p.anchored = true
		line = strings.TrimLeft(line, "/")
	}
	if line == "" {
		return pattern{}, false, nil
	}
	for _, seg := range strings.Split(line, "/") {
		if seg == "" {
			continue
		}
		seg = fkey(seg)
		g := gseg{s: seg, r: []rune(seg)}
		if !globValid(g.r) {
			return pattern{}, false, errBadPattern
		}
		g.lit = !strings.ContainsAny(seg, "*?[\\")
		g.dstar = seg == "**"
		p.segs = append(p.segs, g)
	}
	if len(p.segs) == 0 {
		return pattern{}, false, nil
	}
	if !p.anchored && len(p.segs) > 1 {
		p.anchored = true
	}
	return p, true, nil
}

type patErr string

func (e patErr) Error() string { return string(e) }

const errBadPattern = patErr("规则语法不完整(悬空的 \\ 或没有闭合的 [)")

// NormalizePatterns 清洗规则列表:去掉空行和行尾的 \r,保持顺序(顺序对 ! 很重要,所以不去重),
// 并返回语法有问题的行。
func NormalizePatterns(lines []string) (clean []string, bad []string) {
	for _, l := range lines {
		l = strings.TrimRight(l, "\r\n")
		if strings.TrimSpace(l) == "" {
			continue
		}
		clean = append(clean, l)
		if _, _, err := parsePattern(l); err != nil {
			bad = append(bad, l)
		}
	}
	return
}

// NormalizeRelPaths 清洗精确路径:统一为 / 分隔、去掉首尾斜杠和 ".."。
func NormalizeRelPaths(in []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, p := range in {
		p = strings.ReplaceAll(strings.TrimSpace(p), "\\", "/")
		p = path.Clean("/" + p)
		p = strings.Trim(p, "/")
		if p == "" || seen[fkey(p)] {
			continue
		}
		seen[fkey(p)] = true
		out = append(out, p)
	}
	sort.Strings(out)
	return out
}

func NewMatcher(paths, patterns []string) *Matcher {
	m := &Matcher{paths: map[string]bool{}}
	for _, p := range NormalizeRelPaths(paths) {
		m.paths[fkey(p)] = true
	}
	for _, l := range patterns {
		if p, ok, err := parsePattern(l); ok && err == nil {
			m.patterns = append(m.patterns, p)
		}
	}
	return m
}

// matchParts 只检查 parts 这一层本身:"path"=精确路径,"pattern"=规则,""=不排除。
// parts 已折叠大小写。规则按顺序评估,最后一条命中的决定结果。
func (m *Matcher) matchParts(parts []string, isDir bool) string {
	if len(m.paths) > 0 && m.paths[strings.Join(parts, "/")] {
		return "path"
	}
	excluded := false
	for i := range m.patterns {
		p := &m.patterns[i]
		if p.dirOnly && !isDir {
			continue
		}
		if p.matches(parts) {
			excluded = !p.neg
		}
	}
	if excluded {
		return "pattern"
	}
	return ""
}

// Match 只检查 rel 这一层本身。
func (m *Matcher) Match(rel string, isDir bool) string {
	if m == nil {
		return ""
	}
	return m.matchParts(strings.Split(fkey(rel), "/"), isDir)
}

// Check 同时检查所有上级目录:任何一级被排除,都返回 "parent"(自身命中则返回自身原因)。
// 上级目录被排除后,里面的内容不能再被 ! 规则找回(与 git 一致)。
func (m *Matcher) Check(rel string, isDir bool) string {
	if m == nil {
		return ""
	}
	parts := strings.Split(fkey(rel), "/")
	for i := 1; i < len(parts); i++ {
		if m.matchParts(parts[:i], true) != "" {
			return "parent"
		}
	}
	return m.matchParts(parts, isDir)
}
