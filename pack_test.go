// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at https://mozilla.org/MPL/2.0/.

package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"fmt"
	"math/rand"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/morsuning/zpaq-go"
)

type tfile struct {
	path    string
	data    []byte
	perm    uint32
	mtime   int64
	compres bool
}

func makePack(t *testing.T, path string, level int, blockSz int, files []tfile) int64 {
	t.Helper()
	w, err := newPackWriter(path, level, blockSz, 2, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	var es []packEntry
	for _, f := range files {
		if err := w.AddRaw(f.data, f.compres); err != nil {
			t.Fatal(err)
		}
		es = append(es, packEntry{Path: f.path, Size: int64(len(f.data)), Perm: f.perm, Mtime: f.mtime})
	}
	size, err := w.Close(es)
	if err != nil {
		t.Fatal(err)
	}
	return size
}

func testFiles() []tfile {
	rng := rand.New(rand.NewSource(7))
	rnd := make([]byte, 90000)
	rng.Read(rnd)
	text := []byte(strings.Repeat("package main\n\nfunc main() { println(\"hello 世界\") }\n", 4000))
	return []tfile{
		{"src/a.go", text, 0o644, 1700000000, true},
		{"src/b.go", text[:70001], 0o755, 1700000100, true},
		{"empty.txt", nil, 0o644, 1700000200, true},
		{"中文/目录/文件 名.txt", []byte("你好,世界"), 0o600, 1600000000, true},
		{"big/random.bin", rnd, 0o644, 1700000300, false},
		{"z/another_empty", []byte{}, 0o644, 1700000400, false},
		{"z/tail.md", []byte("# end\n"), 0o644, 1700000500, true},
	}
}

func checkExtracted(t *testing.T, dir string, files []tfile) {
	t.Helper()
	for _, f := range files {
		p := filepath.Join(dir, filepath.FromSlash(f.path))
		b, err := os.ReadFile(p)
		if err != nil {
			t.Fatalf("%s: %v", f.path, err)
		}
		if !bytes.Equal(b, f.data) {
			t.Fatalf("%s 内容不一致 (%d vs %d)", f.path, len(b), len(f.data))
		}
		fi, _ := os.Stat(p)
		if fi.ModTime().Unix() != f.mtime {
			t.Errorf("%s mtime %d != %d", f.path, fi.ModTime().Unix(), f.mtime)
		}
		if runtime.GOOS != "windows" && uint32(fi.Mode().Perm()) != f.perm|0o600 {
			t.Errorf("%s perm %o != %o", f.path, fi.Mode().Perm(), f.perm|0o600)
		}
	}
}

func TestPackRoundtrip(t *testing.T) {
	files := testFiles()
	for _, level := range []int{LevelCtxmix, LevelZPAQ} {
		for _, bs := range []int{1 << 20, 50000, 4096} { // 小块 => 文件跨块、零字节文件落在块边界
			dir := t.TempDir()
			pk := filepath.Join(dir, "x.sdpk")
			size := makePack(t, pk, level, bs, files)
			if fi, _ := os.Stat(pk); fi.Size() != size {
				t.Fatalf("返回大小 %d != 实际 %d", size, fi.Size())
			}
			pr, err := openPack(pk)
			if err != nil {
				t.Fatalf("level %d block %d: %v", level, bs, err)
			}
			out := filepath.Join(dir, "out")
			n, err := pr.extract(context.Background(), out, nil, "t")
			pr.Close()
			if err != nil || n != len(files) {
				t.Fatalf("level %d block %d: n=%d err=%v", level, bs, n, err)
			}
			checkExtracted(t, out, files)
			t.Logf("level %2d block %7d: pack %d bytes", level, bs, size)
		}
	}
}

// ZPAQ 档位的每个数据段都必须是独立的、标准的 ZPAQ 流:
// 库能解开,官方 zpaq 程序(如果装了)也能解开,而且内容与原始数据一致。
func TestPackZPAQSectionsAreStandard(t *testing.T) {
	files := testFiles()
	dir := t.TempDir()
	pk := filepath.Join(dir, "z.sdpk")
	makePack(t, pk, LevelZPAQ, 50000, files)
	pr, err := openPack(pk)
	if err != nil {
		t.Fatal(err)
	}
	defer pr.Close()
	zbin, _ := exec.LookPath("zpaq")
	n := 0
	for i, sec := range pr.secs {
		if sec.Kind != kindZPAQ {
			continue
		}
		n++
		comp := make([]byte, sec.Comp)
		if _, err := pr.f.ReadAt(comp, pr.offs[i]); err != nil {
			t.Fatal(err)
		}
		raw, err := zpaq.Decompress(nil, comp)
		if err != nil || uint64(len(raw)) != sec.Raw || sha256.Sum256(raw) != sec.Sum {
			t.Fatalf("section %d: err=%v len=%d want %d", i, err, len(raw), sec.Raw)
		}
		if zbin == "" {
			continue
		}
		sd := filepath.Join(dir, fmt.Sprintf("sec%d", i))
		if err := os.Mkdir(sd, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(sd, "s.zpaq"), comp, 0o644); err != nil {
			t.Fatal(err)
		}
		cmd := exec.Command(zbin, "x", "s.zpaq")
		cmd.Dir = sd
		if out, err := cmd.CombinedOutput(); err != nil || !bytes.Contains(out, []byte("all OK")) {
			t.Fatalf("zpaq x section %d: %v\n%s", i, err, out)
		}
		got, err := os.ReadFile(filepath.Join(sd, "s"))
		if err != nil || sha256.Sum256(got) != sec.Sum {
			t.Fatalf("zpaq extracted section %d differs: %v", i, err)
		}
	}
	if n == 0 {
		t.Fatal("没有 ZPAQ 段")
	}
	t.Logf("%d 个 ZPAQ 段;官方 zpaq 参与校验: %v", n, zbin != "")
}

func TestPackEmpty(t *testing.T) {
	dir := t.TempDir()
	pk := filepath.Join(dir, "e.sdpk")
	makePack(t, pk, LevelCtxmix, 1<<20, nil)
	pr, err := openPack(pk)
	if err != nil {
		t.Fatal(err)
	}
	defer pr.Close()
	if n, err := pr.extract(context.Background(), filepath.Join(dir, "o"), nil, "t"); err != nil || n != 0 {
		t.Fatalf("n=%d err=%v", n, err)
	}
}

func TestPackCorruption(t *testing.T) {
	files := testFiles()
	dir := t.TempDir()
	pk := filepath.Join(dir, "x.sdpk")
	makePack(t, pk, LevelCtxmix, 50000, files)
	orig, _ := os.ReadFile(pk)

	try := func(name string, mutate func(b []byte) []byte) {
		b := mutate(append([]byte{}, orig...))
		p := filepath.Join(dir, name+".sdpk")
		os.WriteFile(p, b, 0o644)
		pr, err := openPack(p)
		if err != nil {
			return // 打开阶段就发现问题 ✓
		}
		defer pr.Close()
		out := filepath.Join(dir, name+"_out")
		if _, err := pr.extract(context.Background(), out, nil, "t"); err == nil {
			t.Errorf("%s: 损坏的包没有报错", name)
		}
	}
	try("flip-early", func(b []byte) []byte { b[len(packMagic)+20] ^= 0x01; return b })
	try("flip-mid", func(b []byte) []byte { b[len(b)/2] ^= 0x80; return b })
	try("flip-table", func(b []byte) []byte { b[len(b)-packFooterLen-5] ^= 0x01; return b })
	try("flip-footer", func(b []byte) []byte { b[len(b)-packFooterLen+2] ^= 0x01; return b })
	try("truncate", func(b []byte) []byte { return b[:len(b)-30] })
	try("truncate-half", func(b []byte) []byte { return b[:len(b)/2] })
	try("garbage", func(b []byte) []byte { return []byte("this is not a pack file at all, just text.....") })
	try("bad-magic", func(b []byte) []byte { b[0] = 'X'; return b })
}

func TestPackRefusesUnsafePaths(t *testing.T) {
	for _, bad := range []string{"../evil.txt", "/abs/evil.txt", "a/../../evil.txt", ""} {
		dir := t.TempDir()
		pk := filepath.Join(dir, "x.sdpk")
		makePack(t, pk, LevelCtxmix, 1<<20, []tfile{{bad, []byte("x"), 0o644, 1, false}})
		pr, err := openPack(pk)
		if err != nil {
			t.Fatal(err)
		}
		target := filepath.Join(dir, "target")
		_, err = pr.extract(context.Background(), target, nil, "t")
		pr.Close()
		if err == nil {
			t.Errorf("路径 %q 应被拒绝", bad)
		}
		if _, e := os.Stat(filepath.Join(dir, "evil.txt")); e == nil {
			t.Errorf("路径 %q 写到了目标目录之外", bad)
		}
	}
}

func TestPackCancel(t *testing.T) {
	rng := rand.New(rand.NewSource(3))
	data := make([]byte, 3<<20)
	for i := range data { // 半随机文本,压缩得慢一点
		data[i] = "abcdefghij klmnop\n"[rng.Intn(18)]
	}
	dir := t.TempDir()
	pk := filepath.Join(dir, "x.sdpk")
	var cancelled atomic.Int32
	w, err := newPackWriter(pk, LevelCtxmix, 1<<20, 2, func() bool { return cancelled.Load() != 0 }, nil)
	if err != nil {
		t.Fatal(err)
	}
	go func() { time.Sleep(300 * time.Millisecond); cancelled.Store(1) }()
	var werr error
	for off := 0; off < len(data) && werr == nil; off += 1 << 20 {
		werr = w.AddRaw(data[off:off+1<<20], true)
	}
	if werr == nil {
		_, werr = w.Close([]packEntry{{Path: "a", Size: int64(len(data))}})
	} else {
		w.Abort()
	}
	if werr == nil {
		t.Fatal("取消后应报错")
	}
	if _, err := os.Stat(pk); err == nil {
		t.Error("取消后不应留下半成品文件")
	}
}

func TestPackCompressible(t *testing.T) {
	dir := t.TempDir()
	rng := rand.New(rand.NewSource(1))
	rnd := make([]byte, 400<<10)
	rng.Read(rnd)
	os.WriteFile(filepath.Join(dir, "r.bin"), rnd, 0o644)
	os.WriteFile(filepath.Join(dir, "t.txt"), bytes.Repeat([]byte("hello world\n"), 40000), 0o644)
	if packCompressible(filepath.Join(dir, "r.bin"), ".bin", int64(len(rnd))) {
		t.Error("随机数据应判定为不可压缩")
	}
	if !packCompressible(filepath.Join(dir, "t.txt"), ".txt", 480000) {
		t.Error("文本应判定为可压缩")
	}
	if packCompressible("whatever", ".zip", 10) {
		t.Error(".zip 应直接判定为不可压缩")
	}
}

// SDREAL=<目录> go test -run TestRealExport -v :在真实目录上跑完整导出,打印各档位大小与耗时
func TestRealExport(t *testing.T) {
	dir := os.Getenv("SDREAL")
	if dir == "" {
		t.Skip("未设置 SDREAL")
	}
	for _, mode := range []string{"zpaq", "ctxmix", "none"} {
		tmp := t.TempDir()
		rp := RunParams{P: Profile{Name: "t", Src: dir, Out: filepath.Join(tmp, "out"), Compress: mode},
			StatePath: filepath.Join(tmp, "s.json"), DataDir: tmp}
		t0 := time.Now()
		r, err := RunExport(context.Background(), rp, "export", nil)
		if err != nil {
			t.Fatal(err)
		}
		t1 := time.Now()
		t.Logf("%-5s %6d 文件  原始 %9d → 包 %9d (%.1f%%)  导出 %.1fs", mode, r.AddedN, r.Bytes, r.PackBytes, 100*float64(r.PackBytes)/float64(r.Bytes), t1.Sub(t0).Seconds())
		dst := filepath.Join(tmp, "dst")
		if _, err := ApplyInc(context.Background(), []string{r.IncDir}, dst, true, nil, nil); err != nil {
			t.Fatal(err)
		}
		t.Logf("      应用 %.1fs", time.Since(t1).Seconds())
		if ok, d := sameMap(snapshot(t, dir, func(rel string) bool { return strings.HasPrefix(rel, "out/") }), snapshot(t, dst, nil)); !ok {
			t.Fatalf("还原不一致: %s", d)
		}
	}
}
