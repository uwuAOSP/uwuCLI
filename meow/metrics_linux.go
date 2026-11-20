/*
 * SPDX-FileCopyrightText: The uwuAOSP Project
 * SPDX-License-Identifier: Apache-2.0
 */

package main

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
)

type metricsMsg struct {
	total, available, swapTotal, swapFree int64
	disk                                  int64
	memoryOK, diskOK, cpuOK               bool
	cpuTotal, cpuIdle                     uint64
	counts                                [6]int // C++, C, Kotlin, Java, Rust, R8
	linkers                               int
}

func readMemory() (metricsMsg, error) {
	data, err := os.ReadFile("/proc/meminfo")
	if err != nil {
		return metricsMsg{}, err
	}
	return parseMemoryInfo(string(data)), nil
}

func parseMemoryInfo(data string) metricsMsg {
	m := metricsMsg{}
	for _, line := range strings.Split(data, "\n") {
		f := strings.Fields(line)
		if len(f) < 2 {
			continue
		}
		n, err := strconv.ParseInt(f[1], 10, 64)
		if err != nil || n < 0 || n > (1<<63-1)/1024 {
			continue
		}
		switch f[0] {
		case "MemTotal:":
			m.total = n * 1024
		case "MemAvailable:":
			m.available = n * 1024
		case "SwapTotal:":
			m.swapTotal = n * 1024
		case "SwapFree:":
			m.swapFree = n * 1024
		}
	}
	m.memoryOK = m.total > 0
	return m
}

func sampleMetrics(root int, out string) metricsMsg {
	m, _ := readMemory()
	if data, err := os.ReadFile("/proc/stat"); err == nil {
		first, _, _ := strings.Cut(string(data), "\n")
		f := strings.Fields(first)
		if len(f) >= 5 && f[0] == "cpu" {
			m.cpuOK = true
			for i := 1; i < len(f) && i <= 8; i++ {
				n, err := strconv.ParseUint(f[i], 10, 64)
				if err != nil {
					m.cpuOK = false
				}
				m.cpuTotal += n
				if i == 4 {
					m.cpuIdle = n
				}
			}
		}
	}
	var disk syscall.Statfs_t
	if syscall.Statfs(out, &disk) == nil {
		m.disk = int64(disk.Bavail) * int64(disk.Bsize)
		m.diskOK = true
	}
	if root > 0 {
		counts := compilerCounts(root)
		copy(m.counts[:], counts[:6])
		m.linkers = counts[6]
	}
	return m
}

func processParent(stat string) (int, bool) {
	// comm is parenthesized and may contain spaces or ')' characters.
	i := strings.LastIndexByte(stat, ')')
	if i < 0 {
		return 0, false
	}
	f := strings.Fields(stat[i+1:])
	if len(f) < 2 {
		return 0, false
	}
	p, err := strconv.Atoi(f[1])
	return p, err == nil
}

func compilerClass(command []byte) int {
	args := bytes.Split(bytes.TrimRight(command, "\x00"), []byte{0})
	if len(args) == 0 {
		return -1
	}
	exe := filepath.Base(string(args[0]))
	switch {
	case exe == "ld.lld" || exe == "lld" || exe == "ld" || exe == "ld.gold" || exe == "rust-lld":
		return 6
	case exe == "rustc":
		return 4
	case exe == "javac":
		return 3
	case strings.HasPrefix(exe, "kotlinc"):
		return 2
	case exe == "clang" || exe == "clang++" || strings.HasPrefix(exe, "clang-"):
		// Do not count clang's driver and cc1 as separate compilation jobs.
		for _, a := range args[1:] {
			if string(a) == "-cc1" || string(a) == "-cc1as" {
				return -1
			}
		}
		if strings.Contains(exe, "++") {
			return 0
		}
		for i, a := range args[1:] {
			if strings.HasSuffix(string(a), ".cpp") || strings.HasSuffix(string(a), ".cc") || strings.HasSuffix(string(a), ".cxx") {
				return 0
			}
			if string(a) == "-x" && i+2 < len(args) && strings.Contains(string(args[i+2]), "++") {
				return 0
			}
		}
		return 1
	case exe == "java":
		for _, a := range args[1:] {
			name := string(a)
			if name == "com.android.tools.r8.R8" || name == "com.android.tools.r8.D8" {
				return 5
			}
			if strings.HasPrefix(name, "org.jetbrains.kotlin.") {
				return 2
			}
			if name == "com.sun.tools.javac.Main" || strings.Contains(name, "turbine") {
				return 3
			}
		}
	}
	return -1
}

func compilerCounts(root int) [7]int {
	var counts [7]int
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return counts
	}
	parents := make(map[int]int)
	for _, entry := range entries {
		pid, err := strconv.Atoi(entry.Name())
		if err != nil {
			continue
		}
		data, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
		if err != nil {
			continue
		}
		if parent, ok := processParent(string(data)); ok {
			parents[pid] = parent
		}
	}
	for pid := range parents {
		p := pid
		for steps := 0; p > 1 && p != root && steps < len(parents); steps++ {
			next := parents[p]
			if next == p {
				break
			}
			p = next
		}
		if p != root {
			continue
		}
		data, err := os.ReadFile(fmt.Sprintf("/proc/%d/cmdline", pid))
		if err != nil {
			continue
		}
		if class := compilerClass(data); class >= 0 {
			counts[class]++
		}
	}
	return counts
}
