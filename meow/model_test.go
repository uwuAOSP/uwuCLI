/*
 * SPDX-FileCopyrightText: The uwuAOSP Project
 * SPDX-License-Identifier: Apache-2.0
 */

package main

import (
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/x/ansi"
)

func fixtureDashboard() *dashboard {
	m := newDashboard(nil, nil, func() {})
	m.chinese = false
	m.color = false
	m.preview = true
	m.seedPreview()
	return m
}

func TestDashboardGeometry(t *testing.T) {
	for _, size := range [][2]int{{100, 24}, {160, 48}, {40, 12}, {12, 4}, {1, 1}} {
		for _, chinese := range []bool{false, true} {
			for _, color := range []bool{false, true} {
				m := fixtureDashboard()
				m.width = size[0]
				m.height = size[1]
				m.chinese = chinese
				m.color = color
				lines := strings.Split(m.View(), "\n")
				if len(lines) != size[1] {
					t.Fatalf("size=%v: %d rows", size, len(lines))
				}
				for _, line := range lines {
					if w := ansi.StringWidth(line); w > size[0]-1 {
						t.Fatalf("size=%v: width %d: %q", size, w, line)
					}
				}
			}
		}
	}
}

func TestDashboardPanelsAndInteractions(t *testing.T) {
	m := fixtureDashboard()
	for _, text := range []string{"uwuAOSP", "uwu_astonc-cp2a-user", "17.0.182", "soong_only", "Launcher3QuickStep", "CPU 47%", "RAM 67%", "Swap", "Disk", "12640/20030", "compile framework component"} {
		if !strings.Contains(m.View(), text) {
			t.Errorf("missing %q", text)
		}
	}
	m.Update(tea.KeyMsg{Type: tea.KeyCtrlP})
	frozen := m.View()
	m.Update(tickMsg(m.now.Add(time.Minute)))
	if m.View() != frozen {
		t.Fatal("copy mode must freeze the displayed page")
	}
	m.Update(tea.KeyMsg{Type: tea.KeyCtrlP})
	if m.copying {
		t.Fatal("copy mode did not resume")
	}
	m.Update(tea.KeyMsg{Type: tea.KeyCtrlA})
	if !m.details {
		t.Fatal("logs not enabled")
	}
	m.Update(tea.MouseMsg{Button: tea.MouseButtonWheelUp})
	if view := m.View(); !strings.Contains(view, "CPU 47%") || !strings.Contains(view, "12640/20030") || strings.Contains(view, "[Main]") {
		t.Fatal("scrolling logs must retain status and progress without phase tabs")
	}
}

func TestPhaseAndConnectionOwnership(t *testing.T) {
	m := newDashboard(nil, nil, func() {})
	m.status(statusMsg{1, statusEvent{Type: "phase", Description: "graph"}})
	m.status(statusMsg{2, statusEvent{Type: "analysis_stage", Description: "Generating rules", Total: 11}})
	m.status(statusMsg{2, statusEvent{Type: "analysis_progress", Completed: 5, Total: 10}})
	m.status(statusMsg{2, statusEvent{Type: "analysis_progress", Completed: 4, Total: 10}})
	if m.stage == nil || m.stage.done != 5 {
		t.Fatal("analysis progress went backwards")
	}
	m.status(statusMsg{3, statusEvent{Type: "phase", Description: "main"}})
	m.status(statusMsg{1, statusEvent{Type: "start", ID: 1, Description: "late bootstrap"}})
	if m.active != 1 || m.tasks[0].state != succeeded {
		t.Fatal("late graph event stole main phase")
	}
	m.status(statusMsg{3, statusEvent{Type: "start", ID: 1, Description: "first"}})
	m.status(statusMsg{4, statusEvent{Type: "start", ID: 1, Description: "second"}})
	m.status(statusMsg{3, statusEvent{Type: "finish", ID: 1}})
	m.status(statusMsg{3, statusEvent{Type: "finish", ID: 1}})
	if m.tasks[1].done != 1 {
		t.Fatal("duplicate finish counted twice")
	}
	m.status(statusMsg{4, statusEvent{Type: "disconnect"}})
	for _, a := range m.actions {
		if a.key.connection == 4 {
			t.Fatal("disconnected action retained")
		}
	}
}

func TestAnalysisDoesNotBecomeNinjaProgress(t *testing.T) {
	m := newDashboard(nil, nil, func() {})
	m.status(statusMsg{1, statusEvent{Type: "analysis_stage", Description: "Rules", Completed: 4, Total: 11}})
	m.status(statusMsg{1, statusEvent{Type: "analysis_progress", Completed: 300, Total: 400}})
	if m.tasks[0].total != 0 {
		t.Fatal("module variants counted as Ninja actions")
	}
	if got := m.progressLine(); !strings.Contains(got, "4/11") || !strings.Contains(got, "300/400") {
		t.Fatal(got)
	}
}

func TestBoundedLogsAndControlSequences(t *testing.T) {
	m := fixtureDashboard()
	for i := 0; i < 200; i++ {
		m.output("line")
	}
	if len(m.logs) != 160 {
		t.Fatal("unbounded history")
	}
	if got := cleanText("hello\x1b[2J\x1b]52;c;bad\a world\x1b[31m!"); got != "hello world!" {
		t.Fatalf("unsafe output %q", got)
	}
	if timerColor(0) != "#FFFFFF" || timerColor(150*time.Second) != "#FF2626" {
		t.Fatal("timer palette changed")
	}
}
