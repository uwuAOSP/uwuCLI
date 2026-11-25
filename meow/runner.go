/*
 * SPDX-FileCopyrightText: The uwuAOSP Project
 * SPDX-License-Identifier: Apache-2.0
 */

package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sync"
	"syscall"
	"time"
)

type outputMsg []string
type finishedMsg struct {
	err     error
	elapsed time.Duration
	logPath string
}
type tickMsg time.Time

type buildRunner struct {
	top, out  string
	env, args []string
	logPath   string
	rootPID   int
	mu        sync.RWMutex
}

// capture writes unmodified bytes to disk, while forwarding bounded display
// lines. Both streams share one writer, so exec serializes stdout/stderr.
type capture struct {
	mu       sync.Mutex
	log      io.Writer
	send     func(any)
	pending  []byte
	writeErr error
}

func (c *capture) Write(p []byte) (int, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.writeErr == nil {
		_, c.writeErr = c.log.Write(p)
	}
	var lines []string
	for _, b := range p {
		if b == '\n' || b == '\r' {
			if len(c.pending) > 0 {
				lines = append(lines, string(c.pending))
				c.pending = c.pending[:0]
			}
		} else if len(c.pending) < 16384 {
			c.pending = append(c.pending, b)
		}
	}
	if len(lines) > 0 {
		c.send(outputMsg(lines))
	}
	// Never stop draining a child pipe because the output disk filled up.
	return len(p), nil
}

func (c *capture) finish() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.pending) > 0 {
		c.send(outputMsg{string(c.pending)})
		c.pending = nil
	}
	return c.writeErr
}

func (r *buildRunner) pid() int { r.mu.RLock(); defer r.mu.RUnlock(); return r.rootPID }

func resolveRuna(top, out string) (string, error) {
	if runtime.GOOS != "linux" || runtime.GOARCH != "amd64" {
		return "", fmt.Errorf("Runa builds require a Linux amd64 host")
	}
	for _, path := range []string{
		filepath.Join(top, "vendor", "uwu-prebuilts", "runa", "runa"),
		filepath.Join(out, "host", "linux-x86", "bin", "runa"),
	} {
		if info, err := os.Stat(path); err == nil && info.Mode().IsRegular() && info.Mode().Perm()&0111 != 0 {
			return path, nil
		}
	}
	return "", fmt.Errorf("Runa executable not found in vendor/uwu-prebuilts/runa or %s/host/linux-x86/bin; refusing to fall back to another executor", out)
}

func (r *buildRunner) run(ctx context.Context, send func(any)) finishedMsg {
	started := time.Now()
	result := finishedMsg{}
	run := func() error {
		if err := ctx.Err(); err != nil {
			return err
		}
		runa, err := resolveRuna(r.top, r.out)
		if err != nil {
			return err
		}
		if err := os.MkdirAll(filepath.Join(r.out, "meow"), 0755); err != nil {
			return err
		}
		if r.logPath == "" {
			r.logPath = filepath.Join(r.out, "meow", "build-"+started.Format("20060102-150405.000000000")+".log")
		}
		log, err := os.OpenFile(r.logPath, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
		if err != nil {
			return err
		}
		defer log.Close()
		admission, err := prepareAdmission(ctx, runa, r.out, initialParallelism(r.args))
		if err != nil {
			return err
		}
		defer admission.close()
		env := append([]string(nil), r.env...)
		// Select Soong's Ninja-compatible invocation, backed by Runa for both
		// bootstrap and the main build. Enforce this after all caller settings.
		executorSettings := []struct{ key, value string }{
			{"SOONG_NINJA", "ninja"},
			{"NO_ABFS", "true"},
			{"MEOW_NINJA_BIN", admission.wrapper},
			{"MEOW_DYNAMIC_GLOBS", "true"},
		}
		for _, setting := range executorSettings {
			env = setEnv(env, setting.key, setting.value)
		}
		server, err := listenStatus(send)
		if err != nil {
			send(outputMsg{"meow: live status unavailable: " + err.Error()})
		} else {
			defer server.close()
			env = setEnv(env, "MEOW_STATUS_SOCKET", server.path)
		}
		commandArgs := append([]string{"--make-mode"}, r.args...)
		for _, setting := range executorSettings {
			commandArgs = append(commandArgs, setting.key+"="+setting.value)
		}
		if server != nil {
			commandArgs = append(commandArgs, "MEOW_STATUS_SOCKET="+server.path)
		}
		cmd := exec.Command(filepath.Join(r.top, "build", "soong", "soong_ui.bash"), commandArgs...)
		cmd.Dir, cmd.Env = r.top, env
		cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
		cmd.WaitDelay = 2 * time.Second
		writer := &capture{log: log, send: send}
		cmd.Stdout, cmd.Stderr = writer, writer
		send(parallelismMsg(admission.state.jobs))
		fmt.Fprintf(writer, "meow: Runa runtime admission enabled: initial=-j%d ceiling=-j%d\n", admission.state.jobs, admission.state.ceiling)
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := cmd.Start(); err != nil {
			return err
		}
		r.mu.Lock()
		r.rootPID = cmd.Process.Pid
		r.mu.Unlock()
		admissionContext, stopAdmission := context.WithCancel(ctx)
		admissionStopped := make(chan struct{})
		go func() {
			defer close(admissionStopped)
			admission.monitor(admissionContext, r, writer, send)
		}()
		done := make(chan struct{})
		stopped := make(chan struct{})
		go func() {
			defer close(stopped)
			select {
			case <-done:
				return
			case <-ctx.Done():
			}
			// Signal only this invocation's process group, never other builds.
			for _, step := range []struct {
				signal syscall.Signal
				grace  time.Duration
			}{{syscall.SIGINT, 5 * time.Second}, {syscall.SIGTERM, 3 * time.Second}, {syscall.SIGKILL, time.Second}} {
				_ = syscall.Kill(-cmd.Process.Pid, step.signal)
				timer := time.NewTimer(step.grace)
				select {
				case <-done:
					timer.Stop()
					return
				case <-timer.C:
				}
			}
		}()
		err = cmd.Wait()
		stopAdmission()
		close(done)
		<-stopped
		<-admissionStopped
		r.mu.Lock()
		r.rootPID = 0
		r.mu.Unlock()
		if captureErr := writer.finish(); captureErr != nil {
			send(outputMsg{"meow: output log error: " + captureErr.Error()})
		}
		if errors.Is(err, exec.ErrWaitDelay) {
			send(outputMsg{"meow: closed output pipes retained by a background process"})
			err = nil
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return err
	}
	result.err = run()
	result.elapsed, result.logPath = time.Since(started), r.logPath
	return result
}

func cleanText(text string) string {
	// Discard terminal control sequences, including OSC strings from child
	// output. Build logs retain the original bytes; the dashboard renders data.
	var out bytes.Buffer
	for i := 0; i < len(text); {
		b := text[i]
		i++
		if b == 0x1b {
			if i == len(text) {
				break
			}
			kind := text[i]
			i++
			if kind == '[' {
				for i < len(text) {
					b = text[i]
					i++
					if b >= 0x40 && b <= 0x7e {
						break
					}
				}
			} else if kind == ']' || kind == 'P' || kind == '_' || kind == '^' {
				for i < len(text) {
					if text[i] == 7 {
						i++
						break
					}
					if text[i] == 0x1b && i+1 < len(text) && text[i+1] == '\\' {
						i += 2
						break
					}
					i++
				}
			}
		} else if b == '\t' {
			out.WriteString("    ")
		} else if b >= 32 && b != 127 {
			out.WriteByte(b)
		}
	}
	return string(bytes.ToValidUTF8(out.Bytes(), nil))
}
