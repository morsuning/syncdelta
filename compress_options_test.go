// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at https://mozilla.org/MPL/2.0/.

package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

func TestNormalizeCompress(t *testing.T) {
	for in, want := range map[string]string{
		"zpaq": "zpaq", "ctxmix": "ctxmix", "none": "none",
		// 空值和无法识别的值按默认(通用压缩)处理
		"": "zpaq", "bogus": "zpaq", "ZPAQ": "zpaq", "max": "zpaq", "store": "zpaq",
	} {
		if got := normalizeCompress(in); got != want {
			t.Errorf("normalizeCompress(%q) = %q, want %q", in, got, want)
		}
	}
	for in, want := range map[string]int{"zpaq": LevelZPAQ, "ctxmix": LevelCtxmix, "none": levelCopy, "": LevelZPAQ, "bogus": LevelZPAQ} {
		if got, _ := parseLevel(in); got != want {
			t.Errorf("parseLevel(%q) = %d, want %d", in, got, want)
		}
	}
	if LevelZPAQ != 0 {
		t.Errorf("默认档位应是零值,LevelZPAQ = %d", LevelZPAQ)
	}
}

// 已有的 config.json 必须能读取:三个导出方式保持不变,缺省或无法识别的按默认(通用压缩)处理,
// 其余字段不受影响。
func TestConfigLoadsAndNormalizes(t *testing.T) {
	dir := t.TempDir()
	old := `{"version":1,"profiles":[
	  {"id":"a","name":"a","src":"/s","out":"/o","excludePaths":null,"excludePatterns":null,"rehash":false,"compress":"ctxmix"},
	  {"id":"b","name":"b","src":"/s","out":"/o","excludePaths":null,"excludePatterns":null,"rehash":true,"compress":"zpaq"},
	  {"id":"c","name":"c","src":"/s","out":"/o","excludePaths":null,"excludePatterns":null,"rehash":false,"compress":"none"},
	  {"id":"d","name":"d","src":"/s","out":"/o","excludePaths":null,"excludePatterns":null,"rehash":false,"compress":""},
	  {"id":"e","name":"e","src":"/s","out":"/o","excludePaths":null,"excludePatterns":null,"rehash":false}
	],"applied":{"x|y":"2026-01-01"},"lastInc":"inc_1","lastTarget":"/t"}`
	if err := os.WriteFile(filepath.Join(dir, "config.json"), []byte(old), 0o600); err != nil {
		t.Fatal(err)
	}
	s, err := OpenStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]string{"a": "ctxmix", "b": "zpaq", "c": "none", "d": "zpaq", "e": "zpaq"}
	ps := s.List()
	if len(ps) != len(want) {
		t.Fatalf("%d profiles", len(ps))
	}
	for _, p := range ps {
		if p.Compress != want[p.ID] {
			t.Errorf("profile %s: compress %q, want %q", p.ID, p.Compress, want[p.ID])
		}
	}
	if ps[1].Src != "/s" || !ps[1].Rehash {
		t.Errorf("other fields changed: %+v", ps[1])
	}
	if ln, tg := s.ApplyDefaults(); ln != "inc_1" || tg != "/t" {
		t.Errorf("apply defaults lost: %q %q", ln, tg)
	}
	// 新建任务不指定导出方式时,保存下来的是默认值;之后再读取也不变。
	if _, err := s.Upsert(Profile{ID: "n", Name: "n", Src: "/s", Out: "/o"}); err != nil {
		t.Fatal(err)
	}
	want["n"] = "zpaq"
	b, err := os.ReadFile(filepath.Join(dir, "config.json"))
	if err != nil {
		t.Fatal(err)
	}
	var cfg Config
	if err := json.Unmarshal(b, &cfg); err != nil {
		t.Fatal(err)
	}
	if len(cfg.Profiles) != len(want) {
		t.Fatalf("saved %d profiles", len(cfg.Profiles))
	}
	for _, p := range cfg.Profiles {
		if p.Compress != want[p.ID] {
			t.Errorf("saved profile %s: compress %q, want %q", p.ID, p.Compress, want[p.ID])
		}
	}
}

// 界面上的导出方式只有三个,名字里不带括号说明。
func TestExportMethodOptionsInUI(t *testing.T) {
	b, err := os.ReadFile(filepath.Join("web", "index.html"))
	if err != nil {
		t.Fatal(err)
	}
	m := regexp.MustCompile(`(?s)<select id="inCompress"[^>]*>(.*?)</select>`).FindSubmatch(b)
	if m == nil {
		t.Fatal("没有找到导出方式的下拉框")
	}
	opts := regexp.MustCompile(`<option value="([^"]*)">([^<]*)</option>`).FindAllSubmatch(m[1], -1)
	var got []string
	for _, o := range opts {
		got = append(got, string(o[1])+"="+string(o[2]))
		if strings.ContainsAny(string(o[2]), "()（）") {
			t.Errorf("选项 %q 带有括号说明", o[2])
		}
	}
	if want := "zpaq=通用压缩,ctxmix=极限压缩,none=直接复制"; strings.Join(got, ",") != want {
		t.Errorf("选项 = %v, want %s", got, want)
	}
	for _, old := range []string{"较快", "只打包", ">ZPAQ<", ">ctxmix<", `value="max"`, `value="fast"`, `value="store"`} {
		if strings.Contains(string(b), old) {
			t.Errorf("界面里还留着旧的导出方式: %q", old)
		}
	}
}

// 段类型表:合法类型能解析,未知类型一律按"格式不正确"处理,没有任何旧版本的特殊处理。
func TestPackSectionKinds(t *testing.T) {
	var sum [32]byte
	for _, k := range []byte{3, 9, 255} {
		tb := encodeTable([]packSection{{Kind: k, Raw: 10, Comp: 5, Sum: sum}})
		if _, err := decodeTable(tb); err != errPackBad {
			t.Errorf("kind %d: err = %v, want errPackBad", k, err)
		}
	}
	tb := encodeTable([]packSection{{Kind: kindZPAQ, Raw: 10, Comp: 5, Sum: sum}, {Kind: kindCtxmix, Raw: 10, Comp: 5, Sum: sum}, {Kind: kindStored, Raw: 3, Comp: 3, Sum: sum}})
	if secs, err := decodeTable(tb); err != nil || len(secs) != 3 {
		t.Errorf("valid kinds rejected: %v", err)
	}
	if kindStored != 0 || kindZPAQ != 1 || kindCtxmix != 2 {
		t.Errorf("段类型编号 = %d/%d/%d", kindStored, kindZPAQ, kindCtxmix)
	}
}
