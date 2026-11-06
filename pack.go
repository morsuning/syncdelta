// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at https://mozilla.org/MPL/2.0/.

package main

// pack.go —— 增量包容器 data.sdpk。
//
// 布局:
//   "SDPK\x01"                         魔数+版本
//   数据块 0..n-1                      每块独立压缩(可并行),文件内容按清单顺序首尾相连成一条"固实"数据流
//   清单段                             所有文件的 路径/大小/权限/mtime(同样压缩)
//   块表                               每段:类型 + 原始长度 + 压缩长度 + SHA-256(原始数据)
//   页脚(20 字节)                      块表偏移(8) + 块表长度(4) + 块表 CRC32(4) + "SDPK"
//
// 清单放在最后,是因为导出时文件的实际大小要读完才知道。
// 每个数据段解压后都要核对 SHA-256,所以任何损坏/位翻转都会被发现,而不是悄悄写出错误文件。

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"
	"hash/crc32"
	"io"
	"io/fs"
	"math"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"time"

	"github.com/morsuning/ctxmix"
	"github.com/morsuning/zpaq-go"
)

const (
	packMagic     = "SDPK\x01"
	packEnd       = "SDPK"
	packFooterLen = 20
	packFile      = "data.sdpk"

	kindStored = 0
	kindZPAQ   = 1 // 通用压缩:标准 ZPAQ 流(zpaq 工具可以解开)
	kindCtxmix = 2 // 极限压缩:ctxmix 流(体积最小)

	packMaxSection = 1 << 30
)

// 压缩档位
const (
	LevelZPAQ   = 0 // 通用压缩(标准 ZPAQ 格式),默认
	LevelCtxmix = 1 // 极限压缩(ctxmix)
)

var packBlockSize = 16 << 20 // 测试里会改小

func packWorkers() int {
	n := runtime.NumCPU() - 1
	if n < 1 {
		n = 1
	}
	if n > 6 {
		n = 6
	}
	return n
}

// 按总量选块大小:尽量让每个核都有活干,同时块不要太小(块越大压得越狠)。
func packChooseBlock(total int64, workers int) int {
	b := total / int64(workers)
	if b < 16<<20 {
		b = 16 << 20
	}
	if b > 32<<20 {
		b = 32 << 20
	}
	return int(b)
}

type packEntry struct {
	Path  string
	Size  int64
	Perm  uint32
	Mtime int64 // unix 秒
}

type packSection struct {
	Kind byte
	Raw  uint64
	Comp uint64
	Sum  [32]byte
}

func levelOfKind(k byte) (int, bool) {
	switch k {
	case kindZPAQ:
		return LevelZPAQ, true
	case kindCtxmix:
		return LevelCtxmix, true
	}
	return 0, false
}

// ---------------------------------------------------------------- 清单编码

func putUvarint(b *bytes.Buffer, v uint64) {
	var tmp [binary.MaxVarintLen64]byte
	n := binary.PutUvarint(tmp[:], v)
	b.Write(tmp[:n])
}

func putVarint(b *bytes.Buffer, v int64) {
	var tmp [binary.MaxVarintLen64]byte
	n := binary.PutVarint(tmp[:], v)
	b.Write(tmp[:n])
}

func encodeManifest(es []packEntry) []byte {
	var b bytes.Buffer
	putUvarint(&b, uint64(len(es)))
	var prev int64
	for _, e := range es {
		putUvarint(&b, uint64(len(e.Path)))
		b.WriteString(e.Path)
		putUvarint(&b, uint64(e.Size))
		putUvarint(&b, uint64(e.Perm))
		putVarint(&b, e.Mtime-prev)
		prev = e.Mtime
	}
	return b.Bytes()
}

var errPackBad = errors.New("增量包格式不正确或已损坏")

func decodeManifest(raw []byte) ([]packEntry, error) {
	r := bytes.NewReader(raw)
	n, err := binary.ReadUvarint(r)
	if err != nil || n > uint64(len(raw)) {
		return nil, errPackBad
	}
	es := make([]packEntry, 0, n)
	var prev int64
	for i := uint64(0); i < n; i++ {
		pl, err := binary.ReadUvarint(r)
		if err != nil || pl > uint64(r.Len()) {
			return nil, errPackBad
		}
		pb := make([]byte, pl)
		if _, err := io.ReadFull(r, pb); err != nil {
			return nil, errPackBad
		}
		size, err1 := binary.ReadUvarint(r)
		perm, err2 := binary.ReadUvarint(r)
		dm, err3 := binary.ReadVarint(r)
		if err1 != nil || err2 != nil || err3 != nil || size > 1<<62 {
			return nil, errPackBad
		}
		prev += dm
		es = append(es, packEntry{Path: string(pb), Size: int64(size), Perm: uint32(perm), Mtime: prev})
	}
	if r.Len() != 0 {
		return nil, errPackBad
	}
	return es, nil
}

func encodeTable(secs []packSection) []byte {
	var b bytes.Buffer
	putUvarint(&b, uint64(len(secs)))
	for _, s := range secs {
		b.WriteByte(s.Kind)
		putUvarint(&b, s.Raw)
		putUvarint(&b, s.Comp)
		b.Write(s.Sum[:])
	}
	return b.Bytes()
}

func decodeTable(raw []byte) ([]packSection, error) {
	r := bytes.NewReader(raw)
	n, err := binary.ReadUvarint(r)
	if err != nil || n == 0 || n > uint64(len(raw)) {
		return nil, errPackBad
	}
	secs := make([]packSection, n)
	for i := range secs {
		k, e0 := r.ReadByte()
		rl, e1 := binary.ReadUvarint(r)
		cl, e2 := binary.ReadUvarint(r)
		_, e3 := io.ReadFull(r, secs[i].Sum[:])
		if e0 != nil || e1 != nil || e2 != nil || e3 != nil || rl > packMaxSection || cl > packMaxSection+packMaxSection/8 {
			return nil, errPackBad
		}
		if k != kindStored {
			if _, ok := levelOfKind(k); !ok {
				return nil, errPackBad
			}
		} else if rl != cl {
			return nil, errPackBad
		}
		secs[i].Kind, secs[i].Raw, secs[i].Comp = k, rl, cl
	}
	if r.Len() != 0 {
		return nil, errPackBad
	}
	return secs, nil
}

// ---------------------------------------------------------------- 段压缩/解压

type packRes struct {
	kind byte
	raw  int
	data []byte
	sum  [32]byte
	err  error
}

var errCMCorrupt = errors.New("压缩数据损坏")

// pollCtx 把 "func() bool" 形式的取消检查适配成 context.Context。
// 压缩库只会调用它的 Err(),所以只需要覆盖这一个方法。
type pollCtx struct {
	context.Context
	canceled func() bool
}

func (c pollCtx) Err() error {
	if c.canceled != nil && c.canceled() {
		return context.Canceled
	}
	return nil
}

func packCompressSection(data []byte, store bool, level int, cancel func() bool, tick func(int)) packRes {
	r := packRes{raw: len(data), sum: sha256.Sum256(data)}
	if store || len(data) == 0 || (level != LevelZPAQ && len(data) > ctxmix.MaxBlockSize) {
		r.kind, r.data = kindStored, data
		return r
	}
	ctx := pollCtx{Context: context.Background(), canceled: cancel}
	var comp []byte
	var err error
	if level == LevelZPAQ {
		// 整段作为一个 ZPAQ 块(每段本身就是一个标准的 .zpaq 流)。
		comp, err = zpaq.Compress(nil, data, zpaq.WithLevel(zpaq.Best), zpaq.WithBlockSize(len(data)+1),
			zpaq.WithConcurrency(1), zpaq.WithContext(ctx), zpaq.WithProgress(tick))
	} else {
		comp, err = ctxmix.EncodeBlock(nil, data, ctxmix.Best, ctxmix.WithContext(ctx), ctxmix.WithProgress(tick))
	}
	if err != nil {
		r.err = err
		return r
	}
	if len(comp) >= len(data) {
		r.kind, r.data = kindStored, data
		return r
	}
	if level == LevelZPAQ {
		r.kind = kindZPAQ
	} else {
		r.kind = kindCtxmix
	}
	r.data = comp
	return r
}

func packDecodeSection(s packSection, comp []byte, cancel func() bool, tick func(int)) ([]byte, error) {
	var data []byte
	if s.Kind == kindStored {
		if uint64(len(comp)) != s.Raw {
			return nil, errPackBad
		}
		data = comp
	} else {
		lv, ok := levelOfKind(s.Kind)
		if !ok {
			return nil, errPackBad
		}
		var err error
		ctx := pollCtx{Context: context.Background(), canceled: cancel}
		if lv == LevelZPAQ {
			data, err = zpaq.Decompress(nil, comp, zpaq.WithMaxOutput(int64(s.Raw)), zpaq.WithConcurrency(1),
				zpaq.WithContext(ctx), zpaq.WithProgress(tick))
			if err == nil && uint64(len(data)) != s.Raw {
				return nil, errCMCorrupt
			}
		} else {
			data, err = ctxmix.DecodeBlock(nil, comp, int(s.Raw), ctxmix.Best, ctxmix.WithContext(ctx), ctxmix.WithProgress(tick))
		}
		if err != nil {
			if errors.Is(err, zpaq.ErrCorrupt) || errors.Is(err, zpaq.ErrTooLarge) || errors.Is(err, ctxmix.ErrCorrupt) {
				return nil, errCMCorrupt
			}
			return nil, err
		}
	}
	if sha256.Sum256(data) != s.Sum {
		return nil, errors.New("校验失败:增量包数据已损坏")
	}
	return data, nil
}

// ---------------------------------------------------------------- 写入器

type packWriter struct {
	f       *os.File
	level   int
	blockSz int
	cancel  func() bool
	tick    func(int)
	sem     chan struct{}
	maxQ    int

	cur     []byte
	curComp bool
	queue   []chan packRes
	secs    []packSection
	pos     int64
	err     error
	RawSize int64
}

func newPackWriter(path string, level, blockSz, workers int, cancel func() bool, tick func(int)) (*packWriter, error) {
	f, err := os.Create(path)
	if err != nil {
		return nil, err
	}
	if _, err := f.WriteString(packMagic); err != nil {
		f.Close()
		return nil, err
	}
	if workers < 1 {
		workers = 1
	}
	return &packWriter{f: f, level: level, blockSz: blockSz, cancel: cancel, tick: tick,
		sem: make(chan struct{}, workers), maxQ: workers * 2, pos: int64(len(packMagic))}, nil
}

func (w *packWriter) popWrite() error {
	ch := w.queue[0]
	w.queue = w.queue[1:]
	r := <-ch
	if r.err != nil {
		return r.err
	}
	if _, err := w.f.Write(r.data); err != nil {
		return err
	}
	w.secs = append(w.secs, packSection{Kind: r.kind, Raw: uint64(r.raw), Comp: uint64(len(r.data)), Sum: r.sum})
	w.pos += int64(len(r.data))
	return nil
}

func (w *packWriter) submit(data []byte, store bool) error {
	ch := make(chan packRes, 1)
	w.queue = append(w.queue, ch)
	w.sem <- struct{}{}
	go func() {
		defer func() { <-w.sem }()
		ch <- packCompressSection(data, store, w.level, w.cancel, w.tick)
	}()
	for len(w.queue) > w.maxQ {
		if err := w.popWrite(); err != nil {
			return err
		}
	}
	return nil
}

func (w *packWriter) flushBlock() error {
	if len(w.cur) == 0 {
		return nil
	}
	data := w.cur
	w.cur = nil
	return w.submit(data, !w.curComp)
}

// AddRaw 把 p 追加到数据流。comp=false 表示这批数据不值得压缩(已压缩格式),会单独成块直接存储。
func (w *packWriter) AddRaw(p []byte, comp bool) error {
	if w.err != nil {
		return w.err
	}
	w.RawSize += int64(len(p))
	for len(p) > 0 {
		if len(w.cur) > 0 && w.curComp != comp {
			if err := w.flushBlock(); err != nil {
				w.err = err
				return err
			}
		}
		w.curComp = comp
		n := w.blockSz - len(w.cur)
		if n > len(p) {
			n = len(p)
		}
		w.cur = append(w.cur, p[:n]...)
		p = p[n:]
		if len(w.cur) >= w.blockSz {
			if err := w.flushBlock(); err != nil {
				w.err = err
				return err
			}
		}
	}
	return nil
}

func (w *packWriter) drain() error {
	var first error
	for len(w.queue) > 0 {
		if err := w.popWrite(); err != nil && first == nil {
			first = err
		}
	}
	return first
}

// Abort 放弃并删除半成品。
func (w *packWriter) Abort() {
	for _, ch := range w.queue {
		<-ch
	}
	w.queue = nil
	name := w.f.Name()
	w.f.Close()
	os.Remove(name)
}

// Close 写出清单、块表和页脚,返回最终文件大小。
func (w *packWriter) Close(entries []packEntry) (int64, error) {
	if w.err == nil {
		w.err = w.flushBlock()
	}
	if err := w.drain(); err != nil && w.err == nil {
		w.err = err
	}
	if w.err != nil {
		w.Abort()
		return 0, w.err
	}
	man := packCompressSection(encodeManifest(entries), false, w.level, w.cancel, nil)
	if man.err != nil {
		w.Abort()
		return 0, man.err
	}
	if _, err := w.f.Write(man.data); err != nil {
		w.Abort()
		return 0, err
	}
	w.secs = append(w.secs, packSection{Kind: man.kind, Raw: uint64(man.raw), Comp: uint64(len(man.data)), Sum: man.sum})
	w.pos += int64(len(man.data))

	table := encodeTable(w.secs)
	var foot [packFooterLen]byte
	binary.LittleEndian.PutUint64(foot[0:], uint64(w.pos))
	binary.LittleEndian.PutUint32(foot[8:], uint32(len(table)))
	binary.LittleEndian.PutUint32(foot[12:], crc32.ChecksumIEEE(table))
	copy(foot[16:], packEnd)
	if _, err := w.f.Write(table); err != nil {
		w.Abort()
		return 0, err
	}
	if _, err := w.f.Write(foot[:]); err != nil {
		w.Abort()
		return 0, err
	}
	size := w.pos + int64(len(table)) + packFooterLen
	if err := w.f.Close(); err != nil {
		os.Remove(w.f.Name())
		return 0, err
	}
	return size, nil
}

// ---------------------------------------------------------------- 读取器

type packReader struct {
	f       *os.File
	secs    []packSection // 数据段(不含清单)
	offs    []int64
	Entries []packEntry
	Raw     int64
}

func openPack(path string) (*packReader, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	pr, err := openPackFile(f)
	if err != nil {
		f.Close()
		return nil, err
	}
	return pr, nil
}

func openPackFile(f *os.File) (*packReader, error) {
	st, err := f.Stat()
	if err != nil {
		return nil, err
	}
	size := st.Size()
	if size < int64(len(packMagic))+packFooterLen {
		return nil, errPackBad
	}
	head := make([]byte, len(packMagic))
	if _, err := f.ReadAt(head, 0); err != nil || string(head) != packMagic {
		return nil, errPackBad
	}
	var foot [packFooterLen]byte
	if _, err := f.ReadAt(foot[:], size-packFooterLen); err != nil || string(foot[16:]) != packEnd {
		return nil, errPackBad
	}
	toff := int64(binary.LittleEndian.Uint64(foot[0:]))
	tlen := int64(binary.LittleEndian.Uint32(foot[8:]))
	if toff < int64(len(packMagic)) || tlen <= 0 || toff+tlen+packFooterLen != size {
		return nil, errPackBad
	}
	tb := make([]byte, tlen)
	if _, err := f.ReadAt(tb, toff); err != nil || crc32.ChecksumIEEE(tb) != binary.LittleEndian.Uint32(foot[12:]) {
		return nil, errPackBad
	}
	all, err := decodeTable(tb)
	if err != nil {
		return nil, err
	}
	pr := &packReader{f: f, secs: all[:len(all)-1]}
	off := int64(len(packMagic))
	for _, s := range all {
		pr.offs = append(pr.offs, off)
		off += int64(s.Comp)
	}
	if off != toff {
		return nil, errPackBad
	}
	// 清单
	ms := all[len(all)-1]
	comp := make([]byte, ms.Comp)
	if _, err := f.ReadAt(comp, pr.offs[len(all)-1]); err != nil {
		return nil, errPackBad
	}
	mraw, err := packDecodeSection(ms, comp, nil, nil)
	if err != nil {
		return nil, err
	}
	pr.Entries, err = decodeManifest(mraw)
	if err != nil {
		return nil, err
	}
	var want, have int64
	for _, e := range pr.Entries {
		want += e.Size
	}
	for _, s := range pr.secs {
		have += int64(s.Raw)
	}
	if want != have {
		return nil, errPackBad
	}
	pr.Raw = have
	return pr, nil
}

func (r *packReader) Close() error { return r.f.Close() }

// 顺序消费解压后的数据流,按清单切成一个个文件写到 target 下。
type packSink struct {
	target  string
	entries []packEntry
	dst     []string
	idx     int
	remain  int64
	f       *os.File
	tmp     string
	Copied  int
	onFile  func(i int, e packEntry)
}

func (s *packSink) open() error {
	e := s.entries[s.idx]
	dst := s.dst[s.idx]
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}
	s.tmp = dst + ".sdtmp"
	f, err := os.OpenFile(s.tmp, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, fs.FileMode(e.Perm&0o777)|0o600)
	if err != nil {
		return err
	}
	s.f, s.remain = f, e.Size
	if s.onFile != nil {
		s.onFile(s.idx, e)
	}
	return nil
}

func (s *packSink) finish() error {
	e := s.entries[s.idx]
	err := s.f.Close()
	s.f = nil
	if err != nil {
		os.Remove(s.tmp)
		return err
	}
	mt := time.Unix(e.Mtime, 0)
	_ = os.Chtimes(s.tmp, mt, mt)
	dst := s.dst[s.idx]
	if err := os.Rename(s.tmp, dst); err != nil {
		_ = os.Remove(dst)
		if err2 := os.Rename(s.tmp, dst); err2 != nil {
			os.Remove(s.tmp)
			return err2
		}
	}
	s.idx++
	s.Copied++
	return nil
}

// 把已经"到头"的文件(含零字节文件)落盘,并打开下一个
func (s *packSink) advance() error {
	for s.f == nil && s.idx < len(s.entries) {
		if err := s.open(); err != nil {
			return err
		}
		if s.remain == 0 {
			if err := s.finish(); err != nil {
				return err
			}
		}
	}
	return nil
}

func (s *packSink) write(p []byte) error {
	for len(p) > 0 {
		if err := s.advance(); err != nil {
			return err
		}
		if s.f == nil {
			return errPackBad
		}
		n := int64(len(p))
		if n > s.remain {
			n = s.remain
		}
		if _, err := s.f.Write(p[:n]); err != nil {
			return err
		}
		s.remain -= n
		p = p[n:]
		if s.remain == 0 {
			if err := s.finish(); err != nil {
				return err
			}
		}
	}
	return nil
}

func (s *packSink) abort() {
	if s.f != nil {
		s.f.Close()
		os.Remove(s.tmp)
		s.f = nil
	}
}

type decRes struct {
	data []byte
	err  error
}

// extract 把整个包解到 target。progress 以文件数计;tickBytes 以解压字节计(可为空)。
func (r *packReader) extract(ctx context.Context, target string, prog Progress, phase string) (int, error) {
	sink := &packSink{target: target, entries: r.Entries, dst: make([]string, len(r.Entries))}
	for i, e := range r.Entries {
		p, ok := safeJoin(target, e.Path)
		if !ok {
			return 0, fmt.Errorf("增量包里有不安全的路径,已拒绝: %q", e.Path)
		}
		sink.dst[i] = p
	}
	var done int64
	cancel := func() bool { return ctx.Err() != nil }
	tick := func(n int) {
		d := atomic.AddInt64(&done, int64(n))
		if prog != nil {
			prog(phase+"(MiB)", int(min64(d, r.Raw)>>20), int(r.Raw>>20)+1, "")
		}
	}
	sink.onFile = func(i int, e packEntry) {
		if prog != nil && i%50 == 0 {
			prog(phase+" "+fmt.Sprintf("%d/%d", i+1, len(r.Entries)), int(min64(atomic.LoadInt64(&done), r.Raw)>>20), int(r.Raw>>20)+1, e.Path)
		}
	}
	workers := packWorkers()
	for _, s := range r.secs {
		if s.Kind == kindZPAQ { // 每块要几百 MB 内存
			workers = min(workers, 4)
			break
		}
	}
	sem := make(chan struct{}, workers)
	var queue []chan decRes
	next := 0
	pop := func() error {
		ch := queue[0]
		queue = queue[1:]
		res := <-ch
		if res.err != nil {
			return res.err
		}
		return sink.write(res.data)
	}
	drain := func() {
		for _, ch := range queue {
			<-ch
		}
		queue = nil
	}
	if err := sink.advance(); err != nil {
		return 0, err
	}
	for next < len(r.secs) {
		if err := ctx.Err(); err != nil {
			drain()
			sink.abort()
			return sink.Copied, err
		}
		i := next
		next++
		ch := make(chan decRes, 1)
		queue = append(queue, ch)
		sem <- struct{}{}
		go func() {
			defer func() { <-sem }()
			s := r.secs[i]
			comp := make([]byte, s.Comp)
			if _, err := r.f.ReadAt(comp, r.offs[i]); err != nil {
				ch <- decRes{err: errPackBad}
				return
			}
			data, err := packDecodeSection(s, comp, cancel, tick)
			ch <- decRes{data: data, err: err}
		}()
		for len(queue) > workers*2 {
			if err := pop(); err != nil {
				drain()
				sink.abort()
				return sink.Copied, err
			}
		}
	}
	for len(queue) > 0 {
		if err := pop(); err != nil {
			drain()
			sink.abort()
			return sink.Copied, err
		}
	}
	if err := sink.advance(); err != nil {
		return sink.Copied, err
	}
	if sink.idx != len(r.Entries) {
		return sink.Copied, errPackBad
	}
	return sink.Copied, nil
}

func min64(a, b int64) int64 {
	if a < b {
		return a
	}
	return b
}

// ---------------------------------------------------------------- 压缩价值判断

var packIncompressibleExt = map[string]bool{
	".zip": true, ".gz": true, ".tgz": true, ".bz2": true, ".xz": true, ".zst": true, ".7z": true, ".rar": true,
	".jar": true, ".war": true, ".apk": true, ".whl": true, ".lz4": true, ".br": true, ".cab": true,
	".jpg": true, ".jpeg": true, ".png": true, ".gif": true, ".webp": true, ".heic": true, ".avif": true,
	".mp3": true, ".aac": true, ".ogg": true, ".opus": true, ".flac": true, ".m4a": true,
	".mp4": true, ".mkv": true, ".mov": true, ".avi": true, ".webm": true, ".m4v": true,
	".woff": true, ".woff2": true, ".docx": true, ".xlsx": true, ".pptx": true, ".odt": true, ".epub": true,
}

// compressible 判断文件是否值得送进 CM:已知压缩格式直接否;其余采样看字节熵。
func packCompressible(abs, ext string, size int64) bool {
	if packIncompressibleExt[strings.ToLower(ext)] {
		return false
	}
	if size < 256<<10 {
		return true
	}
	f, err := os.Open(abs)
	if err != nil {
		return true
	}
	defer f.Close()
	buf := make([]byte, 64<<10)
	n, _ := io.ReadFull(f, buf)
	if n < 4096 {
		return true
	}
	var cnt [256]int
	for _, b := range buf[:n] {
		cnt[b]++
	}
	// 用整数近似熵:sum(c*log2(n/c)) —— 这里只做是否压缩的启发式判断,不影响数据格式,浮点无妨
	var h float64
	for _, c := range cnt {
		if c > 0 {
			p := float64(c) / float64(n)
			h -= p * math.Log2(p)
		}
	}
	return h < 7.6
}
