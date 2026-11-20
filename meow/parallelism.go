/*
 * SPDX-FileCopyrightText: The uwuAOSP Project
 * SPDX-License-Identifier: Apache-2.0
 */

package main

import (
	"context"
	"fmt"
	"io"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"
)

type parallelismMsg int

type admissionSample struct {
	memory          metricsMsg
	psi             float64
	swapIn, swapOut uint64
}

type admissionState struct {
	initial, ceiling, jobs                  int
	pressureSince, healthySince, nextChange time.Time
	previous                                admissionSample
	previousAt                              time.Time
}

// Return a proposal without committing it. The state and UI advance only after
// atomically publishing a valid limit to the executor's control file.
func (s *admissionState) observe(now time.Time, point admissionSample) (int, string) {
	previous, previousAt := s.previous, s.previousAt
	s.previous, s.previousAt = point, now
	m := point.memory
	swapping := false
	if !previousAt.IsZero() && now.After(previousAt) && point.swapIn >= previous.swapIn && point.swapOut >= previous.swapOut {
		seconds := now.Sub(previousAt).Seconds()
		page := float64(os.Getpagesize())
		swapping = float64(point.swapIn-previous.swapIn)*page/seconds >= 4*(1<<20) ||
			float64(point.swapOut-previous.swapOut)*page/seconds >= 16*(1<<20)
	}
	reserve := max(3*giB, m.total/8)
	linkerHeavy := m.available < 2*reserve && point.psi >= 35 && m.linkers >= max(4, (s.initial+2)/3)
	pressured := (m.available < reserve && point.psi >= 20) ||
		(point.psi >= 20 && swapping) ||
		(m.available < m.total/3 && point.psi >= 45) ||
		linkerHeavy || m.available < giB/2
	if pressured {
		s.healthySince = time.Time{}
		if s.pressureSince.IsZero() {
			s.pressureSince = now
		}
		duration := 20 * time.Second
		if point.psi >= 45 || (linkerHeavy && point.psi >= 35) {
			duration = 6 * time.Second
		}
		if m.available < giB/2 {
			duration = 2 * time.Second
		}
		if now.Sub(s.pressureSince) >= duration && !now.Before(s.nextChange) && s.jobs > 1 {
			return max(1, min(s.jobs-1, s.jobs-s.jobs/4)), "pressure"
		}
		return s.jobs, ""
	}
	s.pressureSince = time.Time{}
	limit := s.initial
	// I/O wait is not idle CPU capacity. Above-baseline admission requires at
	// least one genuinely idle logical CPU over the latest sampling interval.
	if s.jobs >= s.initial && !previousAt.IsZero() && m.cpuOK && previous.memory.cpuOK && m.cpuTotal > previous.memory.cpuTotal && m.cpuIdle >= previous.memory.cpuIdle &&
		float64(m.cpuIdle-previous.memory.cpuIdle)*float64(runtime.NumCPU()) >= float64(m.cpuTotal-previous.memory.cpuTotal) {
		limit = s.ceiling
	}
	if now.Before(s.nextChange) {
		return s.jobs, ""
	}
	if m.available < max(6*giB, m.total/5) || point.psi >= 10 || swapping || s.jobs >= limit {
		s.healthySince = time.Time{}
		return s.jobs, ""
	}
	if s.healthySince.IsZero() {
		s.healthySince = now
	}
	if now.Sub(s.healthySince) < 15*time.Second {
		return s.jobs, ""
	}
	return s.jobs + min(limit-s.jobs, max(1, s.initial/6)), "recovery"
}

func readAdmissionSample(root int, out string) (admissionSample, error) {
	point := admissionSample{memory: sampleMetrics(root, out)}
	if !point.memory.memoryOK || point.memory.available < 0 {
		return point, fmt.Errorf("memory telemetry unavailable")
	}
	data, err := os.ReadFile("/proc/pressure/memory")
	if err != nil {
		return point, err
	}
	found := false
	for _, line := range strings.Split(string(data), "\n") {
		fields := strings.Fields(line)
		if len(fields) == 0 || fields[0] != "full" {
			continue
		}
		for _, field := range fields[1:] {
			if raw, ok := strings.CutPrefix(field, "avg10="); ok {
				point.psi, err = strconv.ParseFloat(raw, 64)
				if err != nil || math.IsNaN(point.psi) || math.IsInf(point.psi, 0) || point.psi < 0 || point.psi > 100 {
					return point, fmt.Errorf("invalid memory PSI %q", raw)
				}
				found = true
			}
		}
	}
	if !found {
		return point, fmt.Errorf("memory PSI full avg10 unavailable")
	}
	data, err = os.ReadFile("/proc/vmstat")
	if err != nil {
		return point, err
	}
	seen := 0
	for _, line := range strings.Split(string(data), "\n") {
		fields := strings.Fields(line)
		if len(fields) != 2 || (fields[0] != "pswpin" && fields[0] != "pswpout") {
			continue
		}
		n, err := strconv.ParseUint(fields[1], 10, 64)
		if err != nil {
			return point, err
		}
		if fields[0] == "pswpin" {
			point.swapIn = n
		} else {
			point.swapOut = n
		}
		seen++
	}
	if seen != 2 {
		return point, fmt.Errorf("swap telemetry unavailable")
	}
	return point, nil
}

type runaAdmission struct {
	directory, wrapper, limits, active string
	state                              admissionState
}

func prepareAdmission(ctx context.Context, binary, out string, initial int) (*runaAdmission, error) {
	probe, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	help, _ := exec.CommandContext(probe, binary, "--help").CombinedOutput()
	if !strings.Contains(string(help), "--parallelism-file") {
		return nil, fmt.Errorf("Runa lacks --parallelism-file runtime admission support")
	}
	directory, err := os.MkdirTemp(filepath.Join(out, "meow"), "admission-")
	if err != nil {
		return nil, err
	}
	ceiling := initial + min(initial/2, int(^uint(0)>>1)-initial)
	a := &runaAdmission{directory: directory, wrapper: filepath.Join(directory, "runa-wrapper"), limits: filepath.Join(directory, "jobs"), active: filepath.Join(directory, "active"), state: admissionState{initial: initial, ceiling: ceiling, jobs: initial}}
	if err := a.publish(initial); err != nil {
		a.close()
		return nil, err
	}
	quote := func(s string) string { return "'" + strings.ReplaceAll(s, "'", "'\\''") + "'" }
	// Bootstrap uses the native analysis concurrency. Only the main invocation
	// gets a raised hard ceiling and the independently controlled initial limit.
	script := "#!/bin/bash\n# SPDX-FileCopyrightText: The uwuAOSP Project\n# SPDX-License-Identifier: Apache-2.0\nset -euo pipefail\n" +
		"for arg in \"$@\"; do\n case \"$arg\" in */bootstrap.ninja) exec " + quote(binary) + " \"$@\" ;; esac\ndone\n" +
		"args=()\nwhile (( $# )); do\n case \"$1\" in\n -j|--parallelism-file) shift; if (( $# )); then shift; fi ;;\n -j[0-9]*|--parallelism-file=*) shift ;;\n *) args+=(\"$1\"); shift ;;\n esac\ndone\n" +
		"printf '%s\\n' \"$$\" > " + quote(a.active) + "\nexec " + quote(binary) + " --parallelism-file " + quote(a.limits) + " -j " + strconv.Itoa(ceiling) + " \"${args[@]}\"\n"
	if err := os.WriteFile(a.wrapper, []byte(script), 0700); err != nil {
		a.close()
		return nil, err
	}
	return a, nil
}

func (a *runaAdmission) publish(jobs int) error {
	if jobs < 1 || jobs > a.state.ceiling {
		return fmt.Errorf("invalid admission parallelism %d", jobs)
	}
	if err := os.WriteFile(a.limits+".next", []byte(strconv.Itoa(jobs)+"\n"), 0600); err != nil {
		return err
	}
	return os.Rename(a.limits+".next", a.limits)
}

func (a *runaAdmission) close() { _ = os.RemoveAll(a.directory) }

func (a *runaAdmission) monitor(ctx context.Context, r *buildRunner, log io.Writer, send func(any)) {
	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()
	warned := false
	for {
		select {
		case <-ctx.Done():
			return
		case now := <-ticker.C:
			if _, err := os.Stat(a.active); err != nil {
				continue
			}
			point, err := readAdmissionSample(r.pid(), r.out)
			if err != nil {
				a.state.pressureSince, a.state.healthySince, a.state.previousAt = time.Time{}, time.Time{}, time.Time{}
				if !warned {
					fmt.Fprintf(log, "meow: runtime admission telemetry unavailable; retaining -j%d: %v\n", a.state.jobs, err)
					warned = true
				}
				continue
			}
			warned = false
			jobs, reason := a.state.observe(now, point)
			if jobs == a.state.jobs {
				continue
			}
			if ctx.Err() != nil {
				return
			}
			if err := a.publish(jobs); err != nil {
				a.state.nextChange = now.Add(10 * time.Second)
				a.state.healthySince = time.Time{}
				fmt.Fprintf(log, "meow: cannot change runtime admission; retaining -j%d: %v\n", a.state.jobs, err)
				continue
			}
			previous := a.state.jobs
			a.state.jobs = jobs
			if reason == "pressure" {
				a.state.nextChange = now.Add(20 * time.Second)
			} else {
				a.state.nextChange = now.Add(15 * time.Second)
			}
			send(parallelismMsg(jobs))
			fmt.Fprintf(log, "meow: %s: admission parallelism %d -> %d (ceiling=%d, available=%.1f GiB, memory PSI=%.1f); running tasks are preserved\n", reason, previous, jobs, a.state.ceiling, float64(point.memory.available)/float64(giB), point.psi)
		}
	}
}
