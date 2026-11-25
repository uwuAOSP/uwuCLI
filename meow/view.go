/*
 * SPDX-FileCopyrightText: The uwuAOSP Project
 * SPDX-License-Identifier: Apache-2.0
 */

package main

import (
	"fmt"
	"io"
	"math"
	"strings"
	"time"

	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"
	"github.com/muesli/termenv"
)

const (
	blue       = "#76B1FF"
	pink       = "#FF84C4"
	canvas     = "#0E1323"
	buildBG    = "#172A3D"
	panelBG    = "#2B1E35"
	selectedBG = "#30223A"
	progressBG = "#232B46"
	actionBG   = "#141C30"
)

type painter struct {
	renderer *lipgloss.Renderer
	color    bool
}

func newPainter(color bool) painter {
	r := lipgloss.NewRenderer(io.Discard)
	if color {
		r.SetColorProfile(termenv.TrueColor)
	} else {
		r.SetColorProfile(termenv.Ascii)
	}
	return painter{r, color}
}
func (p painter) text(s, fg string, bold bool) string {
	style := p.renderer.NewStyle().Bold(bold)
	if p.color && fg != "" {
		style = style.Foreground(lipgloss.Color(fg))
	}
	return style.Render(s)
}
func fit(s string, width int) string {
	width = max(0, width)
	s = ansi.Truncate(s, width, "…")
	return s + strings.Repeat(" ", max(0, width-ansi.StringWidth(s)))
}
func (p painter) row(s, bg string, width int) string {
	s = fit(s, width)
	if !p.color {
		return s
	}
	// Lip Gloss wraps nested ANSI text without restoring its outer style after
	// an inner reset. Reapply this row's background before any following text
	// or padding, while still allowing foreground and emphasis to reset.
	background := "\x1b[" + termenv.RGBColor(bg).Sequence(true) + "m"
	s = strings.NewReplacer(
		"\x1b[0m", "\x1b[0m"+background,
		"\x1b[m", "\x1b[m"+background,
		"\x1b[49m", background,
	).Replace(s)
	return p.renderer.NewStyle().Background(lipgloss.Color(bg)).Render(s)
}
func (m *dashboard) tr(en, zh string) string {
	if m.chinese {
		return zh
	}
	return en
}
func (m *dashboard) taskLabel(i int) string {
	if i == 0 {
		return m.tr("Graph", "构建图")
	}
	return m.tr("Main", "主构建")
}
func (m *dashboard) View() string {
	if m.copying && m.frozen != "" {
		return m.frozen
	}
	return m.render()
}

func (m *dashboard) render() string {
	p := newPainter(m.color)
	w := max(0, m.width-1) // Never occupy the terminal's wrap column.
	contentWidth := max(0, m.width-3)
	content := func(s string) string { return ansi.Truncate(s, contentWidth, "…") }
	h := max(1, m.height)
	bottom := h / 3
	if h < 3 {
		bottom = 0
	}
	body := h - bottom
	rows := make([]string, h)
	for i := range rows {
		bg := canvas
		if i == m.selected+1 {
			bg = selectedBG
		}
		if i == body {
			bg = progressBG
		} else if i > body {
			bg = actionBG
		}
		rows[i] = p.row("", bg, w)
	}
	if body > 0 {
		rows[0] = p.row(p.text(m.tr("[Task]", "[任务]"), blue, true), canvas, w)
	}
	for i, t := range m.tasks {
		if i+1 >= body {
			break
		}
		marker := "  "
		if i == m.selected {
			marker = "→ "
		}
		label := m.taskLabel(i)
		fg := ""
		bold := t.state == running || t.state == failed
		if t.state == running {
			fg = blue
		}
		if t.state != running || i != m.active {
			label = fit(label, 9) + " " + m.taskSummary(t)
			if t.state == succeeded {
				fg = "2"
			}
			if t.state == failed {
				fg = "1"
			}
		}
		decorate := func(s string) string {
			if t.state == pending && m.color {
				return p.renderer.NewStyle().Faint(true).Render(s)
			}
			return p.text(s, fg, bold)
		}
		line := decorate(marker + label)
		if i == m.selected {
			line = p.text(marker+m.taskLabel(i), pink, true) + decorate(strings.TrimPrefix(marker+label, marker+m.taskLabel(i)))
		}
		bg := canvas
		if i == m.selected {
			bg = selectedBG
		}
		rows[i+1] = p.row(content(line), bg, w)
	}
	left := w / 2
	right := w - left
	build := m.buildPanel(p, left)
	system := m.systemPanel(p, right)
	panelRows := max(len(build), len(system))
	for i := 0; i < panelRows && i+3 < body; i++ {
		l := p.row("", buildBG, left)
		r := p.row("", canvas, right)
		if i < len(build) {
			l = build[i]
		}
		if i < len(system) {
			r = system[i]
		}
		rows[i+3] = l + r
	}
	if m.details {
		start := 3 + panelRows
		logs := m.tasks[m.selected].logs
		end := max(0, len(logs)-min(m.scroll, len(logs)))
		first := max(0, end-max(0, body-start))
		for i, line := range logs[first:end] {
			rows[start+i] = p.row(content(line), canvas, w)
		}
	}
	if bottom > 0 {
		startup := m.tasks[m.active].state == pending && len(m.actions) == 0 && m.stage == nil
		if startup {
			if bottom >= 4 {
				text := "Starting meow..."
				rows[body+bottom/2] = p.row(strings.Repeat(" ", max(0, (contentWidth-ansi.StringWidth(text))/2))+p.text(text, blue, true), actionBG, w)
			}
		} else if bottom > 1 {
			rows[body] = p.row(content(p.text(m.progressLine(), blue, true)), progressBG, w)
			if m.stage != nil && bottom > 2 {
				rows[body+1] = p.row(content(m.actionLine(p, m.stage.start, m.stageText())), actionBG, w)
			} else {
				row := body + 1
				for _, a := range m.actions {
					if a.phase != m.active {
						continue
					}
					if row >= h-1 {
						break
					}
					rows[row] = p.row(content(m.actionLine(p, a.start, a.text)), actionBG, w)
					row++
				}
				if row == body+1 && row < h-1 && m.description != "" {
					rows[row] = p.row(content("   "+m.description), actionBG, w)
				}
			}
		}
		footer := m.tr("↑↓ Select   Ctrl+A Logs   Ctrl+P Copy   Ctrl+C Detach", "↑↓ 选择   Ctrl+A 日志   Ctrl+P 复制   Ctrl+C 退出查看")
		if m.copying {
			footer = m.tr("Copy mode   Ctrl+P Resume   Ctrl+C Detach", "复制模式   Ctrl+P 返回   Ctrl+C 退出查看")
		}
		if m.stopping {
			footer = m.tr("Stopping build…", "正在停止构建…")
		}
		footer = content(footer)
		if m.color {
			footer = p.renderer.NewStyle().Faint(true).Render(footer)
		}
		rows[h-1] = p.row(footer, actionBG, w)
	}
	return strings.Join(rows, "\n")
}

func (m *dashboard) taskSummary(t task) string {
	elapsed := time.Duration(0)
	if !t.start.IsZero() {
		end := m.now
		if !t.end.IsZero() {
			end = t.end
		}
		elapsed = end.Sub(t.start)
	}
	switch t.state {
	case pending:
		return "□ " + m.tr("pending", "等待")
	case failed:
		return "× " + m.tr("failed", "失败") + "  " + elapsedText(elapsed)
	case succeeded:
		s := "■ " + elapsedText(elapsed)
		if t.total > 0 {
			s += fmt.Sprintf("  %d%% %d/%d", 100*t.done/t.total, t.done, t.total)
		}
		return s
	default:
		spinner := "|/-\\"[int(m.now.UnixMilli()/200)%4]
		return fmt.Sprintf("%c %s  %s  %s=%d", spinner, m.tr("building", "编译中"), elapsedText(elapsed), m.tr("jobs", "并发"), m.jobs)
	}
}
func (m *dashboard) buildPanel(p painter, w int) []string {
	labels := []string{"C++", "C", "Kotlin", "Java", "Rust", "R8"}
	lines := []string{p.row(p.text("  ◆ "+m.tr("BUILD STATUS", "构建情况"), pink, true), buildBG, w)}
	for i := 0; i < 6; i += 2 {
		entry := func(j int) string {
			return p.text(fmt.Sprintf("%-8s", labels[j]), blue, true) + fmt.Sprintf(" %d", m.metrics.counts[j])
		}
		if w < 36 {
			lines = append(lines, p.row("  "+entry(i), buildBG, w), p.row("  "+entry(i+1), buildBG, w))
		} else {
			lines = append(lines, p.row("  "+entry(i)+"    "+entry(i+1), buildBG, w))
		}
	}
	return lines
}
func memoryText(n int64, known bool) string {
	if !known {
		return "—"
	}
	return fmt.Sprintf("%.1f GiB", float64(max(0, n))/float64(giB))
}
func usageBar(percent float64, known bool, w int) string {
	w = max(0, w)
	if !known {
		return strings.Repeat("─", w)
	}
	n := min(w, max(0, int(math.Round(percent*float64(w)/100))))
	return strings.Repeat("━", n) + strings.Repeat("─", w-n)
}
func (m *dashboard) systemPanel(p painter, w int) []string {
	left := w / 2
	right := w - left
	card := func(s string, width int, bg, fg string, bold bool) string {
		return p.row(p.text(" "+ansi.Truncate(s, max(0, width-2), "…"), fg, bold), bg, width)
	}
	percent := func(name string, n float64, known bool) string {
		if !known {
			return name + " —"
		}
		return fmt.Sprintf("%s %.0f%%", name, n)
	}
	mem := float64(0)
	if m.metrics.total > 0 {
		mem = 100 * float64(max(0, m.metrics.total-m.metrics.available)) / float64(m.metrics.total)
	}
	lines := []string{p.row(p.text("  ◆ "+m.tr("SYSTEM", "系统状态"), pink, true), panelBG, w),
		card(percent("CPU", m.cpuPercent, m.cpuKnown), left, "#374A6B", blue, true) + card(percent("MEM", mem, m.metrics.memoryOK), right, "#53405E", pink, true),
		card(usageBar(m.cpuPercent, m.cpuKnown, left-2), left, "#374A6B", blue, true) + card(usageBar(mem, m.metrics.memoryOK, right-2), right, "#53405E", pink, true)}
	swap := "—"
	if m.metrics.memoryOK {
		swap = fmt.Sprintf("%.1f / %.1f GiB", float64(max(0, m.metrics.swapTotal-m.metrics.swapFree))/float64(giB), float64(m.metrics.swapTotal)/float64(giB))
	}
	for _, pair := range [][4]string{{"FREE", "JOBS", memoryText(m.metrics.available, m.metrics.memoryOK), fmt.Sprint(m.jobs)}, {"SWAP", "DISK", swap, memoryText(m.metrics.disk, m.metrics.diskOK)}} {
		lines = append(lines, card(pair[0], left, "#2C2F36", blue, false)+card(pair[1], right, "#2C2F36", blue, false), card(pair[2], left, "#2C2F36", "#E6E8EE", true)+card(pair[3], right, "#2C2F36", "#E6E8EE", true))
	}
	return lines
}
func (m *dashboard) stageText() string {
	text := m.stage.text
	if m.stage.total > 0 {
		text += fmt.Sprintf(" [%d/%d]", m.stage.done, m.stage.total)
	}
	return text
}
func (m *dashboard) progressLine() string {
	t := m.tasks[m.active]
	description := m.description
	if m.stage != nil {
		t.done = m.stage.summaryDone
		t.total = m.stage.summaryTotal
		t.eta = time.Time{}
		description = m.stageText()
	}
	if t.total <= 0 {
		if description != "" {
			return description
		}
		return m.taskLabel(m.active) + " " + m.taskSummary(t)
	}
	text := fmt.Sprintf("[%3d%% %d/%d", t.done*100/t.total, t.done, t.total)
	if !t.eta.IsZero() {
		text += " " + elapsedText(max(time.Duration(0), t.eta.Sub(m.now)).Round(time.Second)) + " remaining"
	}
	text += "]"
	if description != "" {
		text += " " + description
	}
	return text
}
func (m *dashboard) actionLine(p painter, start time.Time, text string) string {
	d := max(time.Duration(0), m.now.Sub(start))
	s := int(d.Seconds())
	return "   " + p.text(fmt.Sprintf("%2d:%02d ", s/60, s%60), timerColor(d), false) + text
}
func timerColor(d time.Duration) string {
	stops := []struct {
		second int
		rgb    [3]int
	}{{0, [3]int{255, 255, 255}}, {10, [3]int{255, 246, 232}}, {30, [3]int{255, 235, 150}}, {60, [3]int{255, 195, 45}}, {100, [3]int{255, 112, 20}}, {150, [3]int{255, 38, 38}}}
	rgb := stops[len(stops)-1].rgb
	for i := 1; i < len(stops); i++ {
		if d.Seconds() <= float64(stops[i].second) {
			a, b := stops[i-1], stops[i]
			f := max(0, (d.Seconds()-float64(a.second))/float64(b.second-a.second))
			for j := range rgb {
				rgb[j] = a.rgb[j] + int(float64(b.rgb[j]-a.rgb[j])*f)
			}
			break
		}
	}
	return fmt.Sprintf("#%02X%02X%02X", rgb[0], rgb[1], rgb[2])
}
