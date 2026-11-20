/*
 * SPDX-FileCopyrightText: The uwuAOSP Project
 * SPDX-License-Identifier: Apache-2.0
 */

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestStatusServerAndOpenClientShutdown(t *testing.T) {
	messages := make(chan any, 8)
	server, err := listenStatus(func(msg any) { messages <- msg })
	if err != nil {
		t.Fatal(err)
	}
	conn, err := net.Dial("unix", server.path)
	if err != nil {
		server.close()
		t.Fatal(err)
	}
	defer conn.Close()
	if err := json.NewEncoder(conn).Encode(statusEvent{Type: "phase", Description: "main"}); err != nil {
		t.Fatal(err)
	}
	select {
	case value := <-messages:
		if msg, ok := value.(statusMsg); !ok || msg.event.Description != "main" {
			t.Fatal(value)
		}
	case <-time.After(time.Second):
		t.Fatal("status not delivered")
	}
	closed := make(chan struct{})
	go func() { server.close(); close(closed) }()
	select {
	case <-closed:
	case <-time.After(time.Second):
		t.Fatal("open client prevented shutdown")
	}
}

func TestCapturePreservesBytesAndPartialLines(t *testing.T) {
	var raw bytes.Buffer
	var lines []string
	c := &capture{log: &raw, send: func(msg any) { lines = append(lines, msg.(outputMsg)...) }}
	for _, part := range []string{"one\r", "two\npar", "tial"} {
		if _, err := c.Write([]byte(part)); err != nil {
			t.Fatal(err)
		}
	}
	if err := c.finish(); err != nil {
		t.Fatal(err)
	}
	if raw.String() != "one\rtwo\npartial" || len(lines) != 3 || lines[2] != "partial" {
		t.Fatalf("raw=%q lines=%q", raw.String(), lines)
	}
}

func TestMetricsParsingAndCompilerClassification(t *testing.T) {
	m := parseMemoryInfo("MemTotal: 1024 kB\nMemAvailable: 512 kB\nSwapTotal: 256 kB\nSwapFree: 0 kB\n")
	if !m.memoryOK || m.total != 1<<20 || m.available != 512<<10 || m.swapFree != 0 {
		t.Fatalf("%+v", m)
	}
	if parent, ok := processParent("123 (name ) with spaces) R 42 0 0"); !ok || parent != 42 {
		t.Fatalf("parent=%d ok=%t", parent, ok)
	}
	for _, tt := range []struct {
		args string
		want int
	}{{"clang++\x00-c\x00a.cc", 0}, {"clang\x00-cc1\x00a.c", -1}, {"sh\x00-c\x00java com.android.tools.r8.R8", -1}, {"java\x00com.android.tools.r8.R8", 5}, {"rustc\x00a.rs", 4}} {
		if got := compilerClass([]byte(tt.args)); got != tt.want {
			t.Errorf("%q: %d want %d", tt.args, got, tt.want)
		}
	}
}

func TestRunnerUsesOneNativeBuildInvocation(t *testing.T) {
	top := t.TempDir()
	entry := filepath.Join(top, "build", "soong", "soong_ui.bash")
	if err := os.MkdirAll(filepath.Dir(entry), 0700); err != nil {
		t.Fatal(err)
	}
	script := "#!/bin/sh\nprintf '%s\\n' \"$@\"\nprintf 'budget=%s\\n' \"$SOONG_ANALYSIS_MEMORY_LIMIT_BYTES\"\nexit 7\n"
	if err := os.WriteFile(entry, []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	runa := filepath.Join(top, "vendor", "uwu-prebuilts", "runa", "runa")
	if err := os.MkdirAll(filepath.Dir(runa), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(runa, []byte("#!/bin/sh\necho --parallelism-file\n"), 0700); err != nil {
		t.Fatal(err)
	}
	r := &buildRunner{top: top, out: filepath.Join(top, "out"), env: []string{"SOONG_ANALYSIS_MEMORY_LIMIT_BYTES=123"}, args: []string{"-j8", "SystemUI", "NAME=a b"}}
	result := r.run(context.Background(), func(any) {})
	if result.err == nil {
		t.Fatal("child failure was hidden")
	}
	data, err := os.ReadFile(result.logPath)
	if err != nil {
		t.Fatal(err)
	}
	text := string(data)
	if !strings.Contains(text, "--make-mode\n-j8\nSystemUI\nNAME=a b\n") || !strings.Contains(text, "budget=123\n") {
		t.Fatalf("unexpected invocation: %q", text)
	}
	if r.pid() != 0 {
		t.Fatal("finished process still reported as active")
	}
}

func TestRunnerDoesNotStartCanceledBuild(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	top := t.TempDir()
	r := &buildRunner{top: top, out: filepath.Join(top, "out"), plain: true}
	result := r.run(ctx, func(any) {})
	if !errors.Is(result.err, context.Canceled) {
		t.Fatalf("canceled build: %v", result.err)
	}
}
