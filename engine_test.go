// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at https://mozilla.org/MPL/2.0/.

package main

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"testing"
	"time"
)

func write(t *testing.T, root, rel, content string) {
	t.Helper()
	p := filepath.Join(root, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// 返回目录下所有文件(相对路径 -> 内容)
func snapshot(t *testing.T, root string, skip func(rel string) bool) map[string]string {
	t.Helper()
	out := map[string]string{}
	filepath.Walk(root, func(p string, fi os.FileInfo, err error) error {
		if err != nil || fi.IsDir() {
			return nil
		}
		rel, _ := filepath.Rel(root, p)
		rel = filepath.ToSlash(rel)
		if skip != nil && skip(rel) {
			return nil
		}
		b, _ := os.ReadFile(p)
		out[rel] = string(b)
		return nil
	})
	return out
}

func sameMap(a, b map[string]string) (bool, string) {
	var diffs []string
	for k, v := range a {
		if w, ok := b[k]; !ok {
			diffs = append(diffs, "missing in b: "+k)
		} else if v != w {
			diffs = append(diffs, "differs: "+k)
		}
	}
	for k := range b {
		if _, ok := a[k]; !ok {
			diffs = append(diffs, "extra in b: "+k)
		}
	}
	sort.Strings(diffs)
	return len(diffs) == 0, strings.Join(diffs, "; ")
}

func TestMatcher(t *testing.T) {
	m := NewMatcher([]string{"docs/old", "build/"}, []string{"*.log", "node_modules/", "/root_only.txt", "a/b*.tmp", "# comment", ""})
	cases := []struct {
		rel   string
		isDir bool
		want  string
	}{
		{"docs/old", true, "path"},
		{"docs/old/x.md", false, "parent"},
		{"build", true, "path"},
		{"src/app.log", false, "pattern"},
		{"src/node_modules", true, "pattern"},
		{"src/node_modules", false, ""}, // 末尾 / 只匹配目录
		{"root_only.txt", false, "pattern"},
		{"sub/root_only.txt", false, ""},
		{"a/b1.tmp", false, "pattern"},
		{"x/a/b1.tmp", false, ""},
		{"keep.txt", false, ""},
	}
	for _, c := range cases {
		if got := m.Check(c.rel, c.isDir); got != c.want {
			t.Errorf("Check(%q,%v)=%q want %q", c.rel, c.isDir, got, c.want)
		}
	}
	if _, bad := NormalizePatterns([]string{"ok*", "[abc"}); len(bad) != 1 || bad[0] != "[abc" {
		t.Errorf("bad patterns = %v", bad)
	}
	if got := NormalizeRelPaths([]string{"/a/b/", "a\\b", "../../etc", " c "}); strings.Join(got, ",") != "a/b,c,etc" {
		t.Errorf("NormalizeRelPaths = %v", got)
	}
}

func TestFullWorkflow(t *testing.T) {
	for _, mode := range []string{"zpaq", "ctxmix", "none"} {
		mode := mode
		t.Run(mode, func(t *testing.T) { fullWorkflow(t, mode) })
	}
}

// incContents 返回增量包里的文件内容(无论是 data.sdpk 还是 files/ 目录)
func incContents(t *testing.T, inc string) map[string]string {
	t.Helper()
	if _, err := os.Stat(filepath.Join(inc, packFile)); err != nil {
		return snapshot(t, filepath.Join(inc, "files"), nil)
	}
	pr, err := openPack(filepath.Join(inc, packFile))
	if err != nil {
		t.Fatal(err)
	}
	defer pr.Close()
	tmp := t.TempDir()
	if _, err := pr.extract(context.Background(), tmp, nil, "t"); err != nil {
		t.Fatal(err)
	}
	return snapshot(t, tmp, nil)
}

func fullWorkflow(t *testing.T, mode string) {
	tmp := t.TempDir()
	src, out, dst, state := filepath.Join(tmp, "src"), filepath.Join(tmp, "out"), filepath.Join(tmp, "dst"), filepath.Join(tmp, "data", "s.json")
	write(t, src, "a/1.txt", "one")
	write(t, src, "a/b/2.py", "two")
	write(t, src, "docs/readme.md", "doc")
	write(t, src, "中文/文件.txt", "cn")
	write(t, src, "keep.txt", "keep")
	write(t, src, "skipdir/x.txt", "x")
	write(t, src, "noise.log", "log")
	write(t, src, "out_inside/y.txt", "will be excluded by being the out dir")

	rp := RunParams{P: Profile{Name: "t", Src: src, Out: filepath.Join(src, "out_inside"),
		ExcludePaths: []string{"skipdir"}, ExcludePatterns: []string{"*.log"}, Compress: mode}, StatePath: state, DataDir: filepath.Join(tmp, "data")}
	_ = out
	ctx := context.Background()

	// 1. 首次 = 全量
	r, err := RunExport(ctx, rp, "export", nil)
	if err != nil {
		t.Fatal(err)
	}
	if r.AddedN != 5 || r.ModifiedN != 0 || r.DeletedN != 0 || r.HadBase {
		t.Fatalf("first run: %+v", r)
	}
	inc1 := r.IncDir
	if _, err := ApplyInc(ctx, []string{inc1}, dst, true, nil, nil); err != nil {
		t.Fatal(err)
	}
	skipOut := func(rel string) bool {
		return strings.HasPrefix(rel, "out_inside/") || rel == "noise.log" || strings.HasPrefix(rel, "skipdir/")
	}
	if ok, d := sameMap(snapshot(t, src, skipOut), snapshot(t, dst, nil)); !ok {
		t.Fatalf("after full: %s", d)
	}

	// 2. 无变化
	r, _ = RunExport(ctx, rp, "export", nil)
	if !r.NoChanges {
		t.Fatalf("expected no changes: %+v", r)
	}

	// 3. 仅 touch:内容不变,不应导出
	time.Sleep(10 * time.Millisecond)
	now := time.Now().Add(time.Hour)
	os.Chtimes(filepath.Join(src, "keep.txt"), now, now)
	r, _ = RunExport(ctx, rp, "export", nil)
	if !r.NoChanges {
		t.Fatalf("touch should not export: %+v", r)
	}

	// 4. 新增 / 修改 / 删除 / 改名;被排除的文件变化要被忽略
	write(t, src, "a/new.txt", "new")
	write(t, src, "a/b/2.py", "two changed!")
	os.Remove(filepath.Join(src, "docs", "readme.md"))
	os.Rename(filepath.Join(src, "keep.txt"), filepath.Join(src, "kept.txt"))
	write(t, src, "skipdir/x.txt", "changed but excluded")
	write(t, src, "noise.log", "changed but excluded")
	r, err = RunExport(ctx, rp, "export", nil)
	if err != nil {
		t.Fatal(err)
	}
	if r.AddedN != 2 || r.ModifiedN != 1 || r.DeletedN != 2 {
		t.Fatalf("second run: %+v", r)
	}
	if b, _ := os.ReadFile(filepath.Join(r.IncDir, "CHANGES.txt")); !strings.Contains(string(b), "D\tdocs/readme.md") {
		t.Fatalf("CHANGES: %s", b)
	}
	if got := incContents(t, r.IncDir); len(got) != 3 {
		t.Fatalf("inc files: %v", got)
	}
	if _, err := ApplyInc(ctx, []string{r.IncDir}, dst, true, nil, nil); err != nil {
		t.Fatal(err)
	}
	if ok, d := sameMap(snapshot(t, src, skipOut), snapshot(t, dst, nil)); !ok {
		t.Fatalf("after inc: %s", d)
	}
	if _, err := os.Stat(filepath.Join(dst, "docs")); err == nil {
		t.Errorf("empty dir docs should be pruned on apply")
	}

	// 5. 新增排除规则:已同步过的文件被排除后,不应被当成"删除"
	rp.P.ExcludePaths = []string{"skipdir", "a/b"}
	r, _ = RunExport(ctx, rp, "export", nil)
	if !r.NoChanges || r.Dropped != 1 || r.DeletedN != 0 {
		t.Fatalf("newly excluded: %+v", r)
	}
	if _, err := os.Stat(filepath.Join(dst, "a", "b", "2.py")); err != nil {
		t.Errorf("target file must stay")
	}

	// 6. 回退 / 重做基线
	write(t, src, "late.txt", "late")
	r, _ = RunExport(ctx, rp, "export", nil)
	if r.AddedN != 1 {
		t.Fatalf("%+v", r)
	}
	if err := rollbackState(state); err != nil {
		t.Fatal(err)
	}
	r2, _ := RunScan(ctx, rp, nil)
	if r2.AddedN != 1 { // 回退后 late.txt 又是新增
		t.Fatalf("after rollback: %+v", r2)
	}
	if err := rollbackState(state); err != nil { // 再换回来
		t.Fatal(err)
	}
	r2, _ = RunScan(ctx, rp, nil)
	if r2.AddedN != 0 {
		t.Fatalf("after redo: %+v", r2)
	}

	// 7. 清除基线 -> 全量
	if err := clearState(state); err != nil {
		t.Fatal(err)
	}
	r2, _ = RunScan(ctx, rp, nil)
	if r2.HadBase || r2.AddedN < 5 {
		t.Fatalf("after clear: %+v", r2)
	}
}

func TestApplySafety(t *testing.T) {
	tmp := t.TempDir()
	inc := filepath.Join(tmp, "inc_x")
	os.MkdirAll(filepath.Join(inc, "files"), 0o755)
	write(t, tmp, "victim.txt", "must survive")
	target := filepath.Join(tmp, "target")
	write(t, target, "ok.txt", "bye")
	os.WriteFile(filepath.Join(inc, "DELETED.txt"), []byte("../victim.txt\n/etc/passwd\nok.txt\n"), 0o644)
	res, err := ApplyInc(context.Background(), []string{inc}, target, true, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(tmp, "victim.txt")); err != nil {
		t.Fatal("path traversal deleted a file outside target!")
	}
	if _, err := os.Stat(filepath.Join(target, "ok.txt")); err == nil {
		t.Error("ok.txt should be deleted")
	}
	if len(res.Items[0].Skipped) != 2 {
		t.Errorf("skipped = %v", res.Items[0].Skipped)
	}
	// 来源文件夹不能作为目标
	os.WriteFile(filepath.Join(inc, "META.json"), []byte(`{"version":1,"source":"`+filepath.ToSlash(target)+`"}`), 0o644)
	if _, err := ApplyInc(context.Background(), []string{inc}, target, true, nil, nil); err == nil {
		t.Error("applying onto the source folder must be refused")
	}
}

func TestCancelLeavesNoPartialInc(t *testing.T) {
	tmp := t.TempDir()
	src, out := filepath.Join(tmp, "src"), filepath.Join(tmp, "out")
	write(t, src, "a.txt", "a")
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	rp := RunParams{P: Profile{Src: src, Out: out}, StatePath: filepath.Join(tmp, "s.json")}
	if _, err := RunExport(ctx, rp, "export", nil); err == nil {
		t.Fatal("expected cancel error")
	}
	if ents, _ := os.ReadDir(out); len(ents) != 0 {
		t.Errorf("partial inc left behind: %v", ents)
	}
	if _, err := os.Stat(rp.StatePath); err == nil {
		t.Error("baseline must not be written on failure")
	}
	_ = bytes.MinRead
}

// ---------------------------------------------------------------- .gitignore 语义

func TestGitignoreSemantics(t *testing.T) {
	type tc struct {
		rel   string
		isDir bool
		want  bool // 是否被排除
	}
	groups := []struct {
		name  string
		rules []string
		cases []tc
	}{
		{"negate", []string{"*.log", "!keep.log"}, []tc{{"a.log", false, true}, {"x/b.log", false, true}, {"keep.log", false, false}, {"x/keep.log", false, false}}},
		{"dir-only", []string{"build/"}, []tc{{"build", true, true}, {"build", false, false}, {"build/x.o", false, true}, {"src/build", true, true}, {"src/build", false, false}}},
		{"anchored", []string{"/build", "doc/frotz/"}, []tc{{"build", true, true}, {"src/build", true, false}, {"doc/frotz", true, true}, {"a/doc/frotz", true, false}}},
		{"doublestar", []string{"**/frotz", "a/**/b", "abc/**"}, []tc{
			{"frotz", true, true}, {"x/y/frotz", false, true}, {"a/b", false, true}, {"a/x/b", false, true}, {"a/x/y/b", false, true}, {"ab", false, false},
			{"abc", true, false}, {"abc/x", false, true}, {"abc/d/e", false, true}}},
		{"git-doc-reinclude", []string{"/*", "!/foo", "/foo/*", "!/foo/bar"}, []tc{
			{"top.txt", false, true}, {"foo", true, false}, {"foo/other", false, true}, {"foo/bar", true, false}, {"foo/bar/x.txt", false, false}, {"foo/other/y", false, true}}},
		{"parent-excluded", []string{"build/", "!build/keep.txt"}, []tc{{"build/keep.txt", false, true}}},
		{"classes", []string{"t[0-9].c", "u[!0-9].c", "v[^0-9].c", "w?.c"}, []tc{
			{"t5.c", false, true}, {"tx.c", false, false}, {"ux.c", false, true}, {"u5.c", false, false}, {"vq.c", false, true}, {"v5.c", false, false}, {"wz.c", false, true}, {"w.c", false, false}}},
		{"escapes", []string{`\#hash`, `\!bang`, `sp\ ace`, "trail   ", "# comment", ""}, []tc{
			{"#hash", false, true}, {"!bang", false, true}, {"sp ace", false, true}, {"trail", false, true}, {"comment", false, false}}},
		{"star-no-slash", []string{"a/*.txt"}, []tc{{"a/b.txt", false, true}, {"a/b/c.txt", false, false}, {"x/a/b.txt", false, false}}},
		{"all-but-go", []string{"*", "!*/", "!*.go"}, []tc{{"main.go", false, false}, {"x/y/z.go", false, false}, {"readme.md", false, true}, {"x/y/z.txt", false, true}}},
		{"unicode", []string{"ñ*", "Ünï/"}, []tc{{"ñandú.txt", false, true}, {"Ünï", true, true}, {"Ünï/a", false, true}, {"n.txt", false, false}}},
	}
	if runtime.GOOS == "windows" || runtime.GOOS == "darwin" {
		t.Skip("大小写折叠平台上的语义另测")
	}
	for _, g := range groups {
		m := NewMatcher(nil, g.rules)
		for _, c := range g.cases {
			if got := m.Check(c.rel, c.isDir) != ""; got != c.want {
				t.Errorf("[%s] %q isDir=%v: excluded=%v want %v", g.name, c.rel, c.isDir, got, c.want)
			}
		}
	}
	if _, bad := NormalizePatterns([]string{"ok*", "[abc", `trailing\`}); len(bad) != 2 {
		t.Errorf("bad = %v", bad)
	}
	// 顺序敏感:重复的规则不能被去重
	if c, _ := NormalizePatterns([]string{"*.log", "!a.log", "*.log"}); len(c) != 3 {
		t.Errorf("规则不应去重: %v", c)
	}
}

// 与真实的 git 对拍:同一棵目录树、同一组规则,被 git 忽略的文件集合必须和我们的一致。
func TestGitignoreMatchesGit(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("只在 Linux 上对拍(大小写敏感)")
	}
	gitBin, err := exec.LookPath("git")
	if err != nil {
		t.Skip("没有 git")
	}
	files := []string{
		"a.log", "keep.log", "b.txt", "build/out.o", "build/sub/x.o", "src/main.go", "src/util.go", "src/build",
		"src/a/b/c.txt", "src/a/x/b/c.txt", "src/b/c.txt", "doc/frotz/a.md", "doc/other/frotz/b.md", "doc/x.md",
		"foo/bar/baz.txt", "foo/bar2/q.txt", "foo/top.txt", "node_modules/p/i.js", "sub/node_modules/q/j.js",
		"sp ace.txt", "#hash.txt", "!bang.txt", "abc/def/ghi.txt", "abc/x.txt", "Ünï/ñ.txt", "ñandú.txt",
		"test1.txt", "test2.txt", "testA.txt", "test.txt", "a1b", "aXXb", "x/a1b/y.txt", "top.cfg", "x/y/z/deep.cfg",
	}
	sets := [][]string{
		{"*.log", "!keep.log"}, {"build/"}, {"build"}, {"/build"}, {"doc/frotz/"}, {"frotz/"}, {"**/frotz"},
		{"src/a/**/c.txt"}, {"abc/**"}, {"/*", "!/foo", "/foo/*", "!/foo/bar"}, {"test?.txt"}, {"test[0-9].txt"},
		{"test[!0-9].txt"}, {"test[^0-9].txt"}, {`\#hash.txt`}, {`\!bang.txt`}, {"#hash.txt"}, {"!bang.txt"},
		{"sp ace.txt"}, {`sp\ ace.txt`}, {"foo  "}, {"node_modules"}, {"/node_modules"}, {"*"}, {"*", "!*/", "!*.go"},
		{"foo/*", "!foo/bar"}, {"foo/**/baz.txt"}, {"**/baz.txt"}, {"**"}, {"foo/**"}, {"*.txt", "!src/**"},
		{"a*b"}, {"ñ*", "Ünï/"}, {"*.cfg", "!/top.cfg"}, {"/x/**/deep.cfg"}, {"**/y/z/*.cfg"}, {"*.log", "!keep.log", "*.log"},
		{"b*", "!build/", "build/*", "!build/sub"}, {"doc/**/frotz/"}, {"doc/*/frotz"}, {"a?b", "!a1b"},
	}
	tmp := t.TempDir()
	repo := filepath.Join(tmp, "repo")
	for _, f := range files {
		write(t, repo, f, "x")
	}
	run := func(args ...string) []byte {
		cmd := exec.Command(gitBin, append([]string{"-c", "core.excludesFile=/dev/null", "-c", "core.ignorecase=false", "-c", "core.quotepath=off"}, args...)...)
		cmd.Dir = repo
		cmd.Env = append(os.Environ(), "HOME="+tmp, "GIT_CONFIG_NOSYSTEM=1")
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
		return out
	}
	run("init", "-q")
	for _, rules := range sets {
		if err := os.WriteFile(filepath.Join(repo, ".gitignore"), []byte(strings.Join(rules, "\n")+"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		out := run("ls-files", "-z", "--others", "--exclude-standard")
		gitKeep := map[string]bool{}
		for _, p := range strings.Split(string(out), "\x00") {
			if p != "" && p != ".gitignore" {
				gitKeep[p] = true
			}
		}
		m := NewMatcher(nil, rules)
		for _, f := range files {
			mine := m.Check(f, false) == ""
			if mine != gitKeep[f] {
				t.Errorf("规则 %q: 文件 %q 我们 保留=%v, git 保留=%v", rules, f, mine, gitKeep[f])
			}
		}
	}
}
