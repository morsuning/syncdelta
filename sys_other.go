// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at https://mozilla.org/MPL/2.0/.

//go:build !windows

package main

import (
	"fmt"
	"os"
	"os/exec"
)

func hideWindow(c *exec.Cmd) {}

func showError(msg string) { fmt.Fprintln(os.Stderr, msg) }
