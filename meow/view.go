/*
 * SPDX-FileCopyrightText: The uwuAOSP Project
 * SPDX-License-Identifier: Apache-2.0
 */

package main

import (
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"
	"github.com/muesli/termenv"
)

const (
	blue       = "#76B1FF"
	resourceFG = "#83D8C6"
	jobsFG     = "#C6B6F7"
	overviewBG = "#101725"
	overviewFG = "#DCE5F1"
	mutedFG    = "#8996AC"
	progressBG = "#232B46"
	actionBG   = "#141C30"
)

type painter struct {
	renderer *lipgloss.Renderer
	color    bool
	surface  bool
}

func newPainter(color bool) painter {
	r := lipgloss.NewRenderer(io.Discard)
	if color {
		r.SetColorProfile(termenv.TrueColor)
	} else {
		r.SetColorProfile(termenv.Ascii)
	}
	return painter{renderer: r, color: color}
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
	if !p.color || bg == "" {
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
func (m *dashboard) View() string {
	if m.copying && m.frozen != "" {
		return m.frozen
	}
	return m.render()
}

func (m *dashboard) brand(p painter) string {
	const wordmark = "uwuAOSP"
	if !p.color {
		return wordmark
	}
	if !m.trueColor {
		return "\x1b[1;36m" + wordmark + "\x1b[0m"
	}
	// Match the per-letter palette in uwu's banner.
	colors := [...]string{"#8DE3FD", "#92D8FC", "#97CEFB", "#9BC3FB", "#A6CBFC", "#B1D3FC", "#BCDBFD"}
	var b strings.Builder
	for i := range wordmark {
		b.WriteString(p.text(wordmark[i:i+1], colors[i], true))
	}
	return b.String()
}

func (p painter) dim(s string) string {
	if p.surface {
		return p.text(s, mutedFG, false)
	}
	if p.color {
		return p.renderer.NewStyle().Faint(true).Render(s)
	}
	return s
}

func (p painter) value(s string) string {
	if p.surface {
		return p.text(s, overviewFG, false)
	}
	return s
}

func (m *dashboard) summaryRows(p painter, width, height int) []string {
	resources := m.resourceRows(p)
	resources = resources[:min(len(resources), max(0, height-2))]
	headerHeight := height - len(resources)
	rows := []string{"", "  " + m.brand(p)}
	if height >= 10 {
		rows = append(rows, "")
	}
	value := func(key, fallback string) string {
		if v := m.buildConfig[key]; v != "" {
			return strings.TrimSpace(cleanText(v))
		}
		return strings.TrimSpace(cleanText(fallback))
	}
	product := value("TARGET_PRODUCT", m.session.Product)
	release := value("TARGET_RELEASE", m.session.Release)
	variant := value("TARGET_BUILD_VARIANT", m.session.Variant)
	android := value("PLATFORM_VERSION", "")
	if android != "" && release != "" {
		android += " (" + release + ")"
	}
	lunch := ""
	if product != "" && release != "" && variant != "" {
		lunch = strings.Join([]string{product, release, variant}, "-")
	}
	const indent = 12
	field := func(label, value string) string {
		if value == "" {
			return ""
		}
		return p.dim(fit(label, indent)) + p.value(value)
	}
	pair := func(left, right string) {
		switch {
		case left == "" || right == "":
			rows = append(rows, left+right)
		case width < 72:
			rows = append(rows, left, right)
		default:
			rows = append(rows, fit(left, 38)+right)
		}
	}
	if android != "" || value("UWU_VERSION", "") != "" {
		pair(field("android", android), field("release", value("UWU_VERSION", "")))
	}
	if product != "" || value("TARGET_ARCH", "") != "" {
		pair(field("product", product), field("arch", value("TARGET_ARCH", "")))
	}
	if lunch != "" {
		rows = append(rows, field("lunch", lunch))
	}
	pair(field("backend", "runa"), field("soong_only", value("soong_only", "")))

	if m.configured {
		targets := m.targets
		if len(targets) == 0 {
			targets = []string{m.tr("default", "默认")}
		}
		// Reserve room for at least the first target row on short terminals.
		if headerHeight > 2 && len(rows) >= headerHeight {
			rows = rows[:headerHeight-1]
		}
		prefix := p.dim(fit("targets", indent))
		line := ""
		for _, target := range targets {
			target = strings.TrimSpace(cleanText(target))
			if target == "" {
				continue
			}
			next := strings.TrimSpace(line + " " + target)
			if line != "" && ansi.StringWidth(next) > width-indent {
				if len(rows) >= headerHeight-1 {
					line = ansi.Truncate(line, max(0, width-indent-2), "…") + " …"
					break
				}
				rows = append(rows, prefix+p.value(line))
				prefix, next = strings.Repeat(" ", indent), target
			}
			line = next
		}
		if line != "" {
			rows = append(rows, prefix+p.value(line))
		}
	}
	rows = rows[:min(headerHeight, len(rows))]
	if len(rows) < headerHeight && len(resources) > 0 {
		rows = append(rows, "")
	}
	return append(rows, resources...)
}

func (m *dashboard) resourceRows(p painter) []string {
	cpu, ram, swap := "—", "—", "—"
	if m.cpuKnown {
		cpu = fmt.Sprintf("%.0f%%", m.cpuPercent)
	}
	if m.metrics.memoryOK {
		used := max(0, m.metrics.total-m.metrics.available)
		if m.metrics.total > 0 {
			ram = fmt.Sprintf("%.0f%% (%.1f / %.1f GiB)", 100*float64(used)/float64(m.metrics.total),
				float64(used)/float64(giB), float64(m.metrics.total)/float64(giB))
		}
		swap = fmt.Sprintf("%.1f / %.1f GiB", float64(max(0, m.metrics.swapTotal-m.metrics.swapFree))/float64(giB),
			float64(m.metrics.swapTotal)/float64(giB))
	}
	counts := m.metrics.counts
	metric := func(label, value, accent string) string {
		if accent == "" {
			return p.dim(label) + " " + p.value(value)
		}
		return p.dim(label) + " " + p.text(value, accent, true)
	}
	compilerLabels := [...]string{"C++", "C", "Kotlin", "Java", "Rust", "R8"}
	workers := []string{metric(m.tr("Jobs", "并发"), fmt.Sprint(m.jobs), jobsFG)}
	for i, label := range compilerLabels {
		workers = append(workers, metric(label, fmt.Sprint(counts[i]), ""))
	}
	return []string{
		strings.Join([]string{metric("CPU", cpu, resourceFG), metric("RAM", ram, resourceFG),
			metric("Swap", swap, ""), metric(m.tr("Disk", "磁盘"), memoryText(m.metrics.disk, m.metrics.diskOK), "")}, "    "),
		workers[0] + "    " + strings.Join(workers[1:], "  "),
	}
}

func (m *dashboard) render() string {
	p := newPainter(m.color)
	p.surface = m.color && m.trueColor
	bodyBG := ""
	if p.surface {
		bodyBG = overviewBG
	}
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
		bg := bodyBG
		if i == body {
			bg = progressBG
		} else if i > body {
			bg = actionBG
		}
		rows[i] = p.row("", bg, w)
	}
	summaryHeight := body
	if m.details {
		summaryHeight -= min(4, max(0, body-3))
	}
	summary := m.summaryRows(p, contentWidth, summaryHeight)
	for i, line := range summary {
		rows[i] = p.row(content(line), bodyBG, w)
	}
	if m.details && len(summary) < body {
		start := len(summary)
		capacity := body - start
		if capacity > 1 {
			rows[start] = p.row(content(p.dim(m.tr("Logs", "日志"))), bodyBG, w)
			start++
			capacity--
		}
		end := max(min(len(m.logs), capacity), len(m.logs)-m.scroll)
		first := max(0, end-capacity)
		for i, line := range m.logs[first:end] {
			rows[start+i] = p.row(content(p.value(line)), bodyBG, w)
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
					rows[row] = p.row(content("   "+p.value(m.description)), actionBG, w)
				}
			}
		}
		footer := m.tr("Ctrl+A Logs   Ctrl+P Copy   Ctrl+C Detach", "Ctrl+A 日志   Ctrl+P 复制   Ctrl+C 退出查看")
		if m.details {
			footer = m.tr("↑↓ Scroll   Ctrl+A Hide logs   Ctrl+P Copy   Ctrl+C Detach", "↑↓ 滚动   Ctrl+A 隐藏日志   Ctrl+P 复制   Ctrl+C 退出查看")
		}
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
func memoryText(n int64, known bool) string {
	if !known {
		return "—"
	}
	return fmt.Sprintf("%.1f GiB", float64(max(0, n))/float64(giB))
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
		return m.taskSummary(t)
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
	return "   " + p.text(fmt.Sprintf("%2d:%02d ", s/60, s%60), timerColor(d), false) + p.value(text)
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
