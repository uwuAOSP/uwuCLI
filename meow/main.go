/*
 * SPDX-FileCopyrightText: The uwuAOSP Project
 * SPDX-License-Identifier: Apache-2.0
 */

package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"strconv"
	"syscall"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/x/term"
)

func main() { os.Exit(mainExit()) }

func mainExit() int {
	o, err := parseOptions(os.Args[1:])
	if err != nil {
		fmt.Fprintln(os.Stderr, "meow:", err)
		return 2
	}
	if o.help {
		fmt.Print(usage)
		return 0
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	interactive := !o.plain && term.IsTerminal(os.Stdin.Fd()) && term.IsTerminal(os.Stdout.Fd()) && os.Getenv("TERM") != "dumb" && os.Getenv("TERM") != ""
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
	runner := &buildRunner{top: top, out: outputRoot(top, env), env: env, args: append(append([]string(nil), o.args...), overrides...), plain: !interactive}
	var result finishedMsg
	if !interactive {
		result = runner.run(ctx, func(any) {})
	} else {
		// Keep producer delivery cancellable even if terminal initialization fails.
		delivery, closeDelivery := context.WithCancel(context.Background())
		defer closeDelivery()
		inbox := make(chan any, 128)
		send := func(msg any) {
			select {
			case inbox <- msg:
			case <-delivery.Done():
			}
		}
		model := newDashboard(inbox, runner, stop)
		model.deliveryDone = delivery.Done()
		program := tea.NewProgram(model, tea.WithAltScreen(), tea.WithMouseCellMotion(), tea.WithoutSignalHandler(), tea.WithFPS(30))
		completed := make(chan finishedMsg, 1)
		ready := make(chan struct{})
		model.launch = func() tea.Msg { close(ready); return nil }
		go func() {
			select {
			case <-ready:
				result := runner.run(ctx, send)
				completed <- result
				send(result)
			case <-delivery.Done():
				completed <- finishedMsg{err: context.Canceled}
			}
		}()
		go func() {
			select {
			case <-ctx.Done():
				program.Send(stopMsg{})
			case <-delivery.Done():
			}
		}()
		_, uiErr := program.Run()
		closeDelivery()
		stop()
		if uiErr != nil {
			fmt.Fprintln(os.Stderr, "meow: dashboard:", uiErr)
		}
		result = <-completed
		if uiErr != nil && result.err == nil {
			result.err = uiErr
		}
		if result.err != nil {
			for _, line := range model.tasks[model.active].logs {
				fmt.Fprintln(os.Stderr, line)
			}
		}
	}
	if result.logPath != "" {
		fmt.Println("meow: output log:", result.logPath)
	}
	if result.err != nil {
		fmt.Fprintln(os.Stderr, "meow:", result.err)
		if errors.Is(result.err, context.Canceled) {
			return 130
		}
		var exit *exec.ExitError
		if errors.As(result.err, &exit) && exit.ExitCode() > 0 {
			return exit.ExitCode()
		}
		return 1
	}
	fmt.Printf("#### build completed successfully (%s) ####\n", elapsedText(result.elapsed))
	return 0
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
