/*
 * SPDX-FileCopyrightText: The uwuAOSP Project
 * SPDX-License-Identifier: Apache-2.0
 */

package main

import (
	"fmt"
	"strings"
	"testing"
	"unicode/utf8"
)

// Interpret SGR state rather than only checking for a background prefix:
// an inner reset can leave trailing cells painted with the terminal default.
func requireBackground(t *testing.T, row, want string) {
	t.Helper()
	background := "default"
	visible := 0
	for len(row) > 0 {
		if strings.HasPrefix(row, "\x1b[") {
			end := strings.IndexByte(row, 'm')
			if end < 0 {
				t.Fatalf("unterminated SGR: %q", row)
			}
			codes := strings.Split(row[2:end], ";")
			for i := 0; i < len(codes); i++ {
				switch codes[i] {
				case "", "0", "49":
					background = "default"
				case "38", "48":
					if i+4 < len(codes) && codes[i+1] == "2" {
						if codes[i] == "48" {
							var r, g, b int
							if _, err := fmt.Sscanf(strings.Join(codes[i+2:i+5], " "), "%d %d %d", &r, &g, &b); err != nil {
								t.Fatal(err)
							}
							background = fmt.Sprintf("#%02X%02X%02X", r, g, b)
						}
						i += 4
					} else if i+2 < len(codes) && codes[i+1] == "5" {
						i += 2
					}
				}
			}
			row = row[end+1:]
			continue
		}
		r, size := utf8.DecodeRuneInString(row)
		if background != want {
			t.Fatalf("visible character %d (%q) has background %s, want %s", visible, r, background, want)
		}
		visible++
		row = row[size:]
	}
	if visible == 0 {
		t.Fatal("row has no visible cells")
	}
}

func TestRowRestoresBackgroundAfterNestedStyles(t *testing.T) {
	p := newPainter(true)
	for _, text := range []string{
		p.text("Starting meow...", blue, true),
		p.text("title", pink, true) + " remaining text",
		p.text(" 0:38 ", timerColor(0), false) + "compile framework component",
		"before\x1b[m after\x1b[49m tail",
		p.text(strings.Repeat("long title ", 20), blue, true),
	} {
		requireBackground(t, p.row(text, actionBG, 80), actionBG)
	}
}

func TestStartupMessageKeepsActionPaneBackground(t *testing.T) {
	m := newDashboard(nil, nil, nil)
	m.color = true
	m.trueColor = true
	rows := strings.Split(m.View(), "\n")
	startupRow := m.height - m.height/3 + (m.height/3)/2
	if !strings.Contains(rows[startupRow], "Starting meow...") {
		t.Fatal("startup message missing")
	}
	requireBackground(t, rows[startupRow], actionBG)
	requireBackground(t, rows[0], overviewBG)
	requireBackground(t, rows[1], overviewBG)
}
