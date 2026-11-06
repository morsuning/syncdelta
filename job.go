// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at https://mozilla.org/MPL/2.0/.

package main

import (
	"context"
	"errors"
	"sync"
	"time"
)

// 同一时间只运行一个后台任务(扫描/导出/应用),界面轮询它的进度。
type Job struct {
	mu      sync.Mutex
	seq     int
	kind    string
	state   string // idle / running / done / error / canceled
	phase   string
	done    int
	total   int
	cur     string
	result  any
	errMsg  string
	started time.Time
	cancel  context.CancelFunc
}

type JobView struct {
	ID      int    `json:"id"`
	Kind    string `json:"kind"`
	State   string `json:"state"`
	Phase   string `json:"phase"`
	Done    int    `json:"done"`
	Total   int    `json:"total"`
	Current string `json:"current"`
	Result  any    `json:"result,omitempty"`
	Error   string `json:"error,omitempty"`
	Elapsed int    `json:"elapsedSec"`
}

func (j *Job) View() JobView {
	j.mu.Lock()
	defer j.mu.Unlock()
	st := j.state
	if st == "" {
		st = "idle"
	}
	el := 0
	if !j.started.IsZero() {
		el = int(time.Since(j.started).Seconds())
	}
	return JobView{ID: j.seq, Kind: j.kind, State: st, Phase: j.phase, Done: j.done, Total: j.total,
		Current: j.cur, Result: j.result, Error: j.errMsg, Elapsed: el}
}

func (j *Job) Running() bool {
	j.mu.Lock()
	defer j.mu.Unlock()
	return j.state == "running"
}

func (j *Job) Cancel() {
	j.mu.Lock()
	defer j.mu.Unlock()
	if j.state == "running" && j.cancel != nil {
		j.cancel()
	}
}

func (j *Job) progress(phase string, done, total int, cur string) {
	j.mu.Lock()
	j.phase, j.done, j.total, j.cur = phase, done, total, cur
	j.mu.Unlock()
}

var errBusy = errors.New("已有任务正在运行,请等待完成或先取消")

// Start 在后台运行 fn。fn 返回的结果在 done 状态下通过 View().Result 取得。
func (j *Job) Start(kind string, fn func(ctx context.Context, prog Progress) (any, error)) error {
	j.mu.Lock()
	if j.state == "running" {
		j.mu.Unlock()
		return errBusy
	}
	ctx, cancel := context.WithCancel(context.Background())
	j.seq++
	j.kind, j.state, j.phase, j.done, j.total, j.cur = kind, "running", "", 0, 0, ""
	j.result, j.errMsg, j.cancel, j.started = nil, "", cancel, time.Now()
	j.mu.Unlock()

	go func() {
		defer cancel()
		var res any
		var err error
		func() {
			defer func() {
				if r := recover(); r != nil {
					err = errors.New("内部错误(已记录到日志)")
					logPanic(r)
				}
			}()
			res, err = fn(ctx, j.progress)
		}()
		j.mu.Lock()
		defer j.mu.Unlock()
		switch {
		case err == nil:
			j.state, j.result = "done", res
		case errors.Is(err, context.Canceled):
			j.state, j.errMsg = "canceled", "已取消"
		default:
			j.state, j.errMsg = "error", err.Error()
			j.result = res
		}
	}()
	return nil
}
