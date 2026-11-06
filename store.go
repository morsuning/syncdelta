// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at https://mozilla.org/MPL/2.0/.

package main

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"sync"
)

// Profile 是一个"同步任务":一个源文件夹 + 导出位置 + 排除规则,各自独立的基线。
type Profile struct {
	ID              string   `json:"id"`
	Name            string   `json:"name"`
	Src             string   `json:"src"`
	Out             string   `json:"out"`
	ExcludePaths    []string `json:"excludePaths"`
	ExcludePatterns []string `json:"excludePatterns"`
	Rehash          bool     `json:"rehash"`
	Compress        string   `json:"compress"` // zpaq(通用压缩,默认) / ctxmix(极限压缩) / none(直接复制)

	// 以下由服务端维护,客户端提交时会被忽略
	LastRun *RunInfo `json:"lastRun,omitempty"`
}

// 导出方式
const (
	compressZPAQ   = "zpaq"   // 通用压缩(标准 ZPAQ 格式),默认
	compressCtxmix = "ctxmix" // 极限压缩
	compressNone   = "none"   // 直接复制成 files 文件夹
)

// normalizeCompress 把配置里的导出方式规整成上面三个值之一,空值和无法识别的值按默认(通用压缩)处理。
func normalizeCompress(c string) string {
	switch c {
	case compressCtxmix, compressNone:
		return c
	}
	return compressZPAQ
}

type RunInfo struct {
	Time     string `json:"time"`
	Mode     string `json:"mode"` // export / baseline
	Added    int    `json:"added"`
	Modified int    `json:"modified"`
	Deleted  int    `json:"deleted"`
	IncDir   string `json:"incDir,omitempty"`
}

type Config struct {
	Version  int               `json:"version"`
	Profiles []Profile         `json:"profiles"`
	Applied  map[string]string `json:"applied"` // key: 目标路径|增量名 -> 时间
	LastInc  string            `json:"lastInc"`
	LastTgt  string            `json:"lastTarget"`
}

type Store struct {
	mu   sync.Mutex
	path string
	cfg  Config
}

func newID() string {
	b := make([]byte, 6)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

func OpenStore(dataDir string) (*Store, error) {
	s := &Store{path: filepath.Join(dataDir, "config.json")}
	s.cfg = Config{Version: 1, Applied: map[string]string{}}
	b, err := os.ReadFile(s.path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return s, nil
		}
		return nil, err
	}
	if err := json.Unmarshal(b, &s.cfg); err != nil {
		// 配置损坏:备份后重新开始,不让程序起不来
		_ = os.Rename(s.path, s.path+".corrupt")
		s.cfg = Config{Version: 1, Applied: map[string]string{}}
	}
	if s.cfg.Applied == nil {
		s.cfg.Applied = map[string]string{}
	}
	for i := range s.cfg.Profiles {
		s.cfg.Profiles[i].Compress = normalizeCompress(s.cfg.Profiles[i].Compress)
	}
	return s, nil
}

func (s *Store) saveLocked() error {
	b, err := json.MarshalIndent(s.cfg, "", "  ")
	if err != nil {
		return err
	}
	tmp := s.path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, s.path)
}

func (s *Store) List() []Profile {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]Profile, len(s.cfg.Profiles))
	copy(out, s.cfg.Profiles)
	return out
}

func (s *Store) Get(id string) (Profile, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, p := range s.cfg.Profiles {
		if p.ID == id {
			return p, true
		}
	}
	return Profile{}, false
}

// Upsert 新建或更新;更新时保留服务端维护的字段。
func (s *Store) Upsert(p Profile) (Profile, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	p.Compress = normalizeCompress(p.Compress)
	if p.ID == "" {
		p.ID = newID()
		p.LastRun = nil
		s.cfg.Profiles = append(s.cfg.Profiles, p)
	} else {
		found := false
		for i := range s.cfg.Profiles {
			if s.cfg.Profiles[i].ID == p.ID {
				p.LastRun = s.cfg.Profiles[i].LastRun
				s.cfg.Profiles[i] = p
				found = true
				break
			}
		}
		if !found {
			p.LastRun = nil
			s.cfg.Profiles = append(s.cfg.Profiles, p)
		}
	}
	return p, s.saveLocked()
}

func (s *Store) SetLastRun(id string, r RunInfo) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := range s.cfg.Profiles {
		if s.cfg.Profiles[i].ID == id {
			rr := r
			s.cfg.Profiles[i].LastRun = &rr
		}
	}
	_ = s.saveLocked()
}

func (s *Store) Delete(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	var keep []Profile
	for _, p := range s.cfg.Profiles {
		if p.ID != id {
			keep = append(keep, p)
		}
	}
	s.cfg.Profiles = keep
	return s.saveLocked()
}

func appliedKey(target, inc string) string { return pkey(target) + "|" + inc }

func (s *Store) IsApplied(target, inc string) (string, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	t, ok := s.cfg.Applied[appliedKey(target, inc)]
	return t, ok
}

func (s *Store) MarkApplied(target, inc, when string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.cfg.Applied[appliedKey(target, inc)] = when
	_ = s.saveLocked()
}

func (s *Store) SetApplyDefaults(inc, target string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.cfg.LastInc, s.cfg.LastTgt = inc, target
	_ = s.saveLocked()
}

func (s *Store) ApplyDefaults() (string, string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.cfg.LastInc, s.cfg.LastTgt
}
