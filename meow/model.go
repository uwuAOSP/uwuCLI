/*
 * SPDX-FileCopyrightText: The uwuAOSP Project
 * SPDX-License-Identifier: Apache-2.0
 */

package main

import (
	"context"
	"fmt"
	"os"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
)

type stopMsg struct{}
type taskState uint8

const (
	pending taskState = iota
	running
	succeeded
	failed
)

type task struct {
	state       taskState
	start, end  time.Time
	done, total int
	eta         time.Time
	logs        []string
}
type actionKey struct{ connection, id uint64 }
type action struct {
	key   actionKey
	phase int
	text  string
	start time.Time
}
type analysisStage struct {
	connection                             uint64
	text                                   string
	start                                  time.Time
	done, total, summaryDone, summaryTotal int
}

type dashboard struct {
	inbox                                 <-chan any
	deliveryDone                          <-chan struct{}
	runner                                *buildRunner
	cancel                                context.CancelFunc
	launch                                tea.Cmd
	preview, stopping                     bool
	width, height, selected, active, jobs int
	details, copying                      bool
	scroll                                int
	frozen                                string
	now                                   time.Time
	tasks                                 [2]task
	connections                           map[uint64]int
	actions                               []action
	stage                                 *analysisStage
	description                           string
	metrics                               metricsMsg
	cpuPercent                            float64
	cpuKnown                              bool
	chinese, color                        bool
}

func newDashboard(inbox <-chan any, r *buildRunner, cancel context.CancelFunc) *dashboard {
	m := &dashboard{inbox: inbox, runner: r, cancel: cancel, width: 100, height: 24, now: time.Now(), connections: make(map[uint64]int), jobs: initialParallelism(nil)}
	for _, key := range []string{"LC_ALL", "LC_MESSAGES", "LANGUAGE", "LANG"} {
		if v := os.Getenv(key); v != "" {
			m.chinese = strings.HasPrefix(strings.ToLower(v), "zh")
			break
		}
	}
	_, noColor := os.LookupEnv("NO_COLOR")
	m.color = !noColor && os.Getenv("TERM") != "dumb"
	if r != nil {
		m.jobs = initialParallelism(r.args)
	}
	return m
}

func (m *dashboard) Init() tea.Cmd {
	cmds := []tea.Cmd{m.tick(), m.sample()}
	if !m.preview && m.launch != nil {
		cmds = append(cmds, m.launch, m.next())
	}
	return tea.Batch(cmds...)
}
func (m *dashboard) next() tea.Cmd {
	return func() tea.Msg {
		select {
		case msg := <-m.inbox:
			return msg
		case <-m.deliveryDone:
			return nil
		}
	}
}
func (m *dashboard) tick() tea.Cmd {
	return tea.Tick(200*time.Millisecond, func(t time.Time) tea.Msg { return tickMsg(t) })
}
func (m *dashboard) sample() tea.Cmd {
	if m.preview || m.runner == nil {
		return nil
	}
	return func() tea.Msg {
		return sampleMetrics(m.runner.pid(), m.runner.out)
	}
}

func (m *dashboard) Update(message tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := message.(type) {
	case tea.WindowSizeMsg:
		m.width = max(1, msg.Width)
		m.height = max(1, msg.Height)
	case tickMsg:
		m.now = time.Time(msg)
		return m, m.tick()
	case metricsMsg:
		previous := m.metrics
		if msg.cpuOK && previous.cpuOK && msg.cpuTotal > previous.cpuTotal && msg.cpuIdle >= previous.cpuIdle {
			m.cpuPercent = 100 * (1 - float64(msg.cpuIdle-previous.cpuIdle)/float64(msg.cpuTotal-previous.cpuTotal))
			m.cpuKnown = true
		}
		m.metrics = msg
		return m, tea.Tick(time.Second, func(time.Time) tea.Msg { return sampleMetrics(m.runner.pid(), m.runner.out) })
	case outputMsg:
		for _, line := range msg {
			m.output(line)
		}
		return m, m.next()
	case statusMsg:
		m.status(msg)
		return m, m.next()
	case parallelismMsg:
		m.jobs = int(msg)
		return m, m.next()
	case finishedMsg:
		for i := range m.tasks {
			if m.tasks[i].state == running {
				m.tasks[i].end = time.Now()
				if msg.err != nil {
					m.tasks[i].state = failed
				} else {
					m.tasks[i].state = succeeded
				}
			}
		}
		return m, tea.Quit
	case stopMsg:
		return m, m.stop()
	case tea.KeyMsg:
		switch msg.String() {
		case "ctrl+c":
			return m, m.stop()
		case "ctrl+a":
			m.details = !m.details
			m.scroll = 0
		case "ctrl+p":
			if m.copying {
				m.copying = false
				m.frozen = ""
				return m, tea.Batch(tea.EnableMouseCellMotion, tea.HideCursor)
			}
			m.copying = true
			m.frozen = m.render()
			return m, tea.Batch(tea.DisableMouse, tea.ShowCursor)
		case "up", "k", "K":
			m.selected = (m.selected + 1) % 2
			m.scroll = 0
		case "down", "j", "J":
			m.selected = (m.selected + 1) % 2
			m.scroll = 0
		case "pgup":
			m.scrollBy(3)
		case "pgdown":
			m.scrollBy(-3)
		case "home":
			m.details = true
			m.scroll = len(m.tasks[m.selected].logs)
		case "end":
			m.scroll = 0
		}
	case tea.MouseMsg:
		if msg.Button == tea.MouseButtonWheelUp || msg.Button == tea.MouseButtonWheelDown {
			m.selected = m.active
			if msg.Button == tea.MouseButtonWheelUp {
				m.scrollBy(3)
			} else {
				m.scrollBy(-3)
			}
		}
	}
	return m, nil
}

func (m *dashboard) stop() tea.Cmd {
	if m.cancel != nil {
		m.cancel()
	}
	if m.preview {
		return tea.Quit
	}
	m.stopping = true
	return nil // The build owns exit; do not leave child processes behind.
}
func (m *dashboard) scrollBy(delta int) {
	m.details = true
	m.scroll = min(len(m.tasks[m.selected].logs), max(0, m.scroll+delta))
}

func (m *dashboard) begin(phase int) {
	if phase == 0 && m.active == 1 {
		return
	} // Late analysis messages cannot steal Main.
	if m.active != phase {
		previous := &m.tasks[m.active]
		if previous.state == running {
			previous.state = succeeded
			previous.end = m.now
		}
		m.stage = nil
		m.description = ""
	}
	m.active = phase
	if m.tasks[phase].state == pending {
		m.tasks[phase].state = running
		m.tasks[phase].start = m.now
	}
}

func (m *dashboard) status(msg statusMsg) {
	e := msg.event
	if e.Type == "phase" {
		phase := 0
		if e.Description == "main" {
			phase = 1
		} else if e.Description != "graph" {
			return
		}
		m.connections[msg.connection] = phase
		m.begin(phase)
		return
	}
	phase, known := m.connections[msg.connection]
	if !known {
		phase = m.active
	}
	started := m.now
	if e.StartedUnixNano > 0 {
		started = time.Unix(0, e.StartedUnixNano)
	}
	switch e.Type {
	case "analysis_stage":
		if m.active == 1 || e.Description == "" {
			return
		}
		m.connections[msg.connection] = 0
		m.begin(0)
		m.stage = &analysisStage{connection: msg.connection, text: cleanText(e.Description), start: started}
		if validCounts(e.Completed, e.Total) {
			m.stage.summaryDone = e.Completed
			m.stage.summaryTotal = e.Total
		}
	case "analysis_summary", "analysis_progress":
		if m.stage == nil || m.stage.connection != msg.connection || !validCounts(e.Completed, e.Total) {
			return
		}
		if e.Type == "analysis_summary" {
			m.stage.summaryDone = e.Completed
			m.stage.summaryTotal = e.Total
		} else {
			if e.Total == m.stage.total && e.Completed < m.stage.done {
				return
			}
			m.stage.done = e.Completed
			m.stage.total = e.Total
		}
	case "analysis_end":
		if m.stage != nil && m.stage.connection == msg.connection {
			m.stage = nil
		}
	case "reset":
		m.begin(phase)
		m.tasks[phase].done = 0
		m.tasks[phase].total = 0
		m.tasks[phase].eta = time.Time{}
	case "total":
		if e.Total > 0 {
			m.tasks[phase].total = e.Total
		}
	case "estimate":
		if e.EstimatedTimeUnixNano > 0 {
			m.tasks[phase].eta = time.Unix(0, e.EstimatedTimeUnixNano)
		}
	case "start":
		key := actionKey{msg.connection, e.ID}
		for _, a := range m.actions {
			if a.key == key {
				return
			}
		}
		m.begin(phase)
		m.actions = append(m.actions, action{key, phase, cleanText(e.Description), started})
		if phase == m.active {
			m.description = cleanText(e.Description)
		}
	case "finish":
		key := actionKey{msg.connection, e.ID}
		for i, a := range m.actions {
			if a.key == key {
				m.actions = append(m.actions[:i], m.actions[i+1:]...)
				m.tasks[a.phase].done++
				break
			}
		}
		if phase == m.active && e.Description != "" {
			m.description = cleanText(e.Description)
		}
	case "disconnect":
		delete(m.connections, msg.connection)
		if m.stage != nil && m.stage.connection == msg.connection {
			m.stage = nil
		}
		kept := m.actions[:0]
		for _, a := range m.actions {
			if a.key.connection != msg.connection {
				kept = append(kept, a)
			}
		}
		m.actions = kept
	}
}
func validCounts(done, total int) bool { return done >= 0 && total > 0 && done <= total }

func (m *dashboard) output(raw string) {
	line := strings.TrimSpace(cleanText(raw))
	if line == "" {
		return
	}
	if strings.Contains(line, "Analyzing Android.bp") && m.active == 0 {
		m.begin(0)
	}
	t := &m.tasks[m.active]
	if len(line) > 4096 {
		line = line[:4096]
		line = strings.ToValidUTF8(line, "")
	}
	if len(t.logs) == 160 {
		copy(t.logs, t.logs[1:])
		t.logs[len(t.logs)-1] = line
	} else {
		t.logs = append(t.logs, line)
	}
	// Text progress is a fallback only. Structured totals count Ninja actions,
	// whereas analysis counters count module variants and must stay separate.
	if len(m.connections) == 0 && m.stage == nil {
		var percent, done, total int
		if n, _ := fmt.Sscanf(line, "[%d%% %d/%d", &percent, &done, &total); n == 3 && validCounts(done, total) {
			m.begin(m.active)
			t.done = done
			t.total = total
			if _, description, ok := strings.Cut(line, "]"); ok {
				m.description = strings.TrimSpace(description)
			}
		}
	}
}

func (m *dashboard) seedPreview() {
	m.active = 1
	m.selected = 1
	m.jobs = 24
	m.tasks[0] = task{state: succeeded, start: m.now.Add(-3 * time.Minute), end: m.now.Add(-2 * time.Minute), done: 1, total: 1}
	m.tasks[1] = task{state: running, start: m.now.Add(-2 * time.Minute), done: 12640, total: 20030, eta: m.now.Add(312 * time.Second)}
	m.metrics = metricsMsg{total: 128 * giB, available: 42 * giB, swapTotal: 32 * giB, swapFree: 27 * giB, disk: 320 * giB, memoryOK: true, diskOK: true, counts: [6]int{16, 2, 3, 2, 1, 2}}
	m.cpuKnown = true
	m.cpuPercent = 47
	m.description = "//frameworks/base:framework-minus-apex"
	m.actions = []action{{key: actionKey{1, 1}, phase: 1, text: "compile framework component", start: m.now.Add(-38 * time.Second)}}
	m.tasks[1].logs = []string{"[ 63% 12640/20030 5m12s remaining] //frameworks/base:framework-minus-apex"}
}
