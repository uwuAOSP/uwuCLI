/*
 * SPDX-FileCopyrightText: The uwuAOSP Project
 * SPDX-License-Identifier: Apache-2.0
 */

package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"syscall"
	"time"

	tea "github.com/charmbracelet/bubbletea"
)

func controlBuild(ctx context.Context, action string, args []string) int {
	if len(args) != 0 && !(action == "attach" && len(args) == 1 && args[0] == "--plain") {
		fmt.Fprintf(os.Stderr, "usage: meow %s", action)
		if action == "attach" {
			fmt.Fprint(os.Stderr, " [--plain]")
		}
		fmt.Fprintln(os.Stderr)
		return 2
	}
	cwd, err := os.Getwd()
	if err != nil {
		return reportError(err)
	}
	env := os.Environ()
	top, err := sourceRoot(cwd, env)
	if err != nil {
		return reportError(err)
	}
	out := outputRoot(top, env)
	var s buildSession
	if err := readJSON(sessionPath(out), &s); err != nil {
		if os.IsNotExist(err) {
			return reportError(fmt.Errorf("no recorded build in %s", out))
		}
		return reportError(err)
	}
	if action == "attach" {
		return attachBuild(ctx, out, s, len(args) == 0 && terminalAvailable())
	}
	snapshot, result, err := observeBuild(ctx, out, s, action)
	if err != nil {
		return reportError(err)
	}
	if action == "stop" {
		switch {
		case result != nil:
			fmt.Println("meow: build already finished")
		case snapshot == nil:
			return reportError(fmt.Errorf("build control unavailable; check meow status"))
		default:
			fmt.Println("meow: stop requested; use meow status to check cleanup")
		}
		return 0
	}

	arguments := "default targets"
	if len(s.Arguments) != 0 {
		arguments = fmt.Sprintf("%q", s.Arguments)
	}
	fmt.Printf("%s: %s (supervisor PID %d)\n", s.Product, arguments, s.GuardPID)
	fmt.Println("meow: output log:", s.Log)
	if result != nil {
		printResult(*result)
		return 0 // Reporting status succeeded regardless of the build's exit code.
	}
	phase := "starting"
	if snapshot == nil {
		phase = "supervisor starting or cleaning up; control unavailable"
	} else if snapshot.Stopping {
		phase = "stopping"
	} else if snapshot.Active == 1 {
		phase = "building"
	} else if snapshot.PID != 0 {
		phase = "analyzing"
	}
	fmt.Printf("%s (%s)", phase, elapsedText(time.Since(s.Started)))
	if snapshot != nil {
		t := snapshot.Tasks[snapshot.Active]
		if snapshot.Stage != nil {
			t.Done, t.Total = snapshot.Stage.Done, snapshot.Stage.Total
		}
		if t.Total > 0 {
			fmt.Printf(" [%d/%d]", t.Done, t.Total)
		}
		if snapshot.Stage != nil {
			fmt.Print(" ", snapshot.Stage.Text)
		} else if snapshot.Description != "" {
			fmt.Print(" ", snapshot.Description)
		}
	}
	fmt.Println()
	return 0
}

func observeBuild(ctx context.Context, out string, s buildSession, action string) (*buildSnapshot, *buildResult, error) {
	var result buildResult
	readResult := func() (*buildResult, error) {
		err := readJSON(s.resultPath(), &result)
		if os.IsNotExist(err) {
			return nil, nil
		}
		return &result, err
	}
	if result, err := readResult(); result != nil || err != nil {
		return nil, result, err
	}
	if snapshot, err := s.request(ctx, action); err == nil {
		return &snapshot, nil, nil
	}
	if ctx.Err() != nil {
		return nil, nil, ctx.Err()
	}
	// The control socket closes before the supervisor finishes cleaning up.
	// Probe the lock without modifying it, and recheck the result after it closes.
	lock, err := os.Open(lockPath(out))
	if err != nil {
		return nil, nil, err
	}
	defer lock.Close()
	err = syscall.Flock(int(lock.Fd()), syscall.LOCK_SH|syscall.LOCK_NB)
	if err != nil && !errors.Is(err, syscall.EWOULDBLOCK) {
		return nil, nil, err
	}
	if result, readErr := readResult(); result != nil || readErr != nil {
		return nil, result, readErr
	}
	var latest buildSession
	if err == nil || (readJSON(sessionPath(out), &latest) == nil && latest.Log != s.Log) {
		return nil, &buildResult{ExitCode: 1, Error: "build supervisor disappeared without recording a result"}, nil
	}
	return nil, nil, nil
}

func printResult(result buildResult) int {
	if result.ExitCode != 0 {
		fmt.Fprintf(os.Stderr, "meow: %s (exit %d)\n", result.Error, result.ExitCode)
	} else {
		fmt.Printf("#### build completed successfully (%s) ####\n", elapsedText(result.Elapsed))
	}
	return result.ExitCode
}

func attachBuild(ctx context.Context, out string, s buildSession, interactive bool) int {
	if !interactive {
		return attachPlain(ctx, out, s)
	}
	ctx, detach := context.WithCancel(ctx)
	defer detach()
	inbox := make(chan any)
	model := newDashboard(inbox, &buildRunner{out: out}, detach)
	model.session = s
	model.deliveryDone = ctx.Done()
	model.requestStop = func() tea.Msg {
		snapshot, result, err := observeBuild(ctx, out, s, "stop")
		if err == nil && snapshot == nil && result == nil {
			err = fmt.Errorf("build control unavailable")
		}
		return stopReplyMsg{stopping: snapshot != nil, err: err}
	}
	program := tea.NewProgram(model, tea.WithContext(ctx), tea.WithAltScreen(), tea.WithMouseCellMotion(), tea.WithoutSignalHandler(), tea.WithFPS(30))
	go func() {
		ticker := time.NewTicker(500 * time.Millisecond)
		defer ticker.Stop()
		for ctx.Err() == nil {
			snapshot, result, err := observeBuild(ctx, out, s, "attach")
			var msg any
			if err != nil {
				msg = sessionEnded{err: err}
			} else if result != nil {
				msg = sessionEnded{result: *result}
			} else if snapshot != nil {
				msg = *snapshot
			}
			if msg != nil {
				select {
				case inbox <- msg:
				case <-ctx.Done():
					return
				}
			}
			if err != nil || result != nil {
				return
			}
			select {
			case <-ticker.C:
			case <-ctx.Done():
				return
			}
		}
	}()
	_, err := program.Run()
	if model.ended != nil {
		fmt.Println("meow: output log:", s.Log)
		if model.ended.err != nil {
			return reportError(model.ended.err)
		}
		if model.ended.result.ExitCode != 0 {
			for _, line := range model.logs {
				fmt.Fprintln(os.Stderr, line)
			}
		}
		return printResult(model.ended.result)
	}
	if err != nil && ctx.Err() == nil {
		return reportError(err)
	}
	fmt.Println("meow: detached; use meow attach to reconnect or meow stop to cancel")
	return 0
}

func attachPlain(ctx context.Context, out string, s buildSession) int {
	var log *os.File
	var offset int64
	defer func() {
		if log != nil {
			_ = log.Close()
		}
	}()
	drain := func() error {
		if log == nil {
			var err error
			log, err = os.Open(s.Log)
			if os.IsNotExist(err) {
				return nil
			}
			if err != nil {
				return err
			}
		}
		info, err := log.Stat()
		if err != nil {
			return err
		}
		n, err := io.CopyN(os.Stdout, log, max(0, info.Size()-offset))
		offset += n
		return err
	}
	ticker := time.NewTicker(250 * time.Millisecond)
	defer ticker.Stop()
	for ctx.Err() == nil {
		_, result, err := observeBuild(ctx, out, s, "status")
		if ctx.Err() != nil {
			break
		}
		if err != nil {
			return reportError(err)
		}
		if err := drain(); err != nil {
			return reportError(err)
		}
		if result != nil {
			fmt.Println("meow: output log:", s.Log)
			return printResult(*result)
		}
		select {
		case <-ticker.C:
		case <-ctx.Done():
		}
	}
	fmt.Fprintln(os.Stderr, "meow: detached; use meow attach to reconnect or meow stop to cancel")
	return 130
}
