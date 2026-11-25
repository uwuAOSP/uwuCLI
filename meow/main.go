/*
 * SPDX-FileCopyrightText: The uwuAOSP Project
 * SPDX-License-Identifier: Apache-2.0
 */

package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/x/term"
)

func main() { os.Exit(mainExit()) }

func mainExit() int {
	args := os.Args[1:]
	if len(args) >= 3 && args[0] == guardMode {
		ready := internalFile(3, "startup")
		defer ready.Close()
		if err := guardBuild(args[1], args[2], args[3:], ready); err != nil {
			_ = json.NewEncoder(ready).Encode(startupReply{Error: err.Error()})
			return 1
		}
		return 0
	}
	if len(args) >= 4 && args[0] == serviceMode {
		r := &buildRunner{top: args[1], out: args[2], logPath: args[3], args: args[4:], env: os.Environ()}
		if err := serveBuild(r, internalFile(3, "control"), internalFile(4, "result"), internalFile(5, "lock")); err != nil {
			return 1
		}
		return 0
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM, syscall.SIGHUP)
	defer stop()
	if len(args) > 0 {
		switch args[0] {
		case "status", "stop", "attach":
			return controlBuild(ctx, args[0], args[1:])
		}
	}
	o, err := parseOptions(args)
	if err != nil {
		fmt.Fprintln(os.Stderr, "meow:", err)
		return 2
	}
	if o.help {
		fmt.Print(usage)
		return 0
	}
	interactive := !o.plain && terminalAvailable()
	if o.preview {
		if !interactive {
			fmt.Fprintln(os.Stderr, "meow: --preview requires an interactive terminal")
			return 2
		}
		model := newDashboard(nil, nil, stop)
		model.preview = true
		model.seedPreview()
		program := tea.NewProgram(model, tea.WithAltScreen(), tea.WithMouseCellMotion(), tea.WithoutSignalHandler(), tea.WithFPS(30))
		go func() { <-ctx.Done(); program.Send(stopMsg{}) }()
		_, err = program.Run()
		if err != nil {
			fmt.Fprintln(os.Stderr, "meow:", err)
			return 1
		}
		return 0
	}
	start, err := os.Getwd()
	if err != nil {
		fmt.Fprintln(os.Stderr, "meow:", err)
		return 1
	}
	env := buildEnvironment(os.Environ(), o.args)
	top, err := sourceRoot(start, env)
	if err != nil {
		fmt.Fprintln(os.Stderr, "meow:", err)
		return 2
	}
	if product, _ := envValue(env, "TARGET_PRODUCT"); product == "" {
		fmt.Fprintln(os.Stderr, "meow: run lunch before building")
		return 2
	}
	memory, _ := readMemory()
	env, overrides, err := configureAnalysis(o, env, memory.total, memory.available)
	if err != nil {
		fmt.Fprintln(os.Stderr, "meow:", err)
		return 2
	}
	env = setEnv(env, "TOP", top)
	env = setEnv(env, "ORIGINAL_PWD", start)
	runner := &buildRunner{top: top, out: outputRoot(top, env), env: env, args: append(append([]string(nil), o.args...), overrides...)}
	session, err := startBuild(ctx, runner)
	if err != nil {
		return reportError(err)
	}
	if o.detach {
		fmt.Println("meow: build started; output log:", session.Log)
		return 0
	}
	return attachBuild(ctx, runner.out, session, interactive)
}

func terminalAvailable() bool {
	return term.IsTerminal(os.Stdin.Fd()) && term.IsTerminal(os.Stdout.Fd()) && os.Getenv("TERM") != "dumb" && os.Getenv("TERM") != ""
}

func reportError(err error) int {
	fmt.Fprintln(os.Stderr, "meow:", err)
	return 1
}

func elapsedText(d time.Duration) string {
	s := max(0, int(d.Seconds()))
	if s >= 3600 {
		return fmt.Sprintf("%dh%02dm%02ds", s/3600, s/60%60, s%60)
	}
	if s >= 60 {
		return fmt.Sprintf("%dm%02ds", s/60, s%60)
	}
	return strconv.Itoa(s) + "s"
}
