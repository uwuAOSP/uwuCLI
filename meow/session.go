/*
 * SPDX-FileCopyrightText: The uwuAOSP Project
 * SPDX-License-Identifier: Apache-2.0
 */

package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

const (
	guardMode   = "--meow-build-guard"
	serviceMode = "--meow-build-service"
	controlWait = 2 * time.Second
)

type buildSession struct {
	Product, Log, Socket string
	Release, Variant     string
	Arguments            []string
	Started              time.Time
	GuardPID             int
}

type buildResult struct {
	ExitCode int
	Error    string
	Elapsed  time.Duration
}

type startupReply struct {
	Session buildSession
	Error   string
}

func sessionPath(out string) string { return filepath.Join(out, "meow", ".session.json") }
func lockPath(out string) string    { return filepath.Join(out, "meow", ".lock") }
func (s buildSession) resultPath() string {
	return strings.TrimSuffix(s.Log, ".log") + ".result.json"
}

func readJSON(path string, dst any) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	return json.NewDecoder(f).Decode(dst)
}

func writeJSON(path string, value any) error {
	f, err := os.CreateTemp(filepath.Dir(path), ".meow-state-")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	err = json.NewEncoder(f).Encode(value)
	if closeErr := f.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		return err
	}
	return os.Rename(f.Name(), path)
}

func selfCommand(args ...string) *exec.Cmd {
	// Use the same binary even if the installed CLI is replaced during a build.
	return exec.Command("/proc/self/exe", args...)
}

func startBuild(ctx context.Context, r *buildRunner) (buildSession, error) {
	if err := ctx.Err(); err != nil {
		return buildSession{}, err
	}
	reader, writer, err := os.Pipe()
	if err != nil {
		return buildSession{}, err
	}
	defer reader.Close()
	defer writer.Close()
	defer context.AfterFunc(ctx, func() { _ = reader.Close() })()
	cmd := selfCommand(append([]string{guardMode, r.top, r.out}, r.args...)...)
	cmd.Dir, cmd.Env = r.top, r.env
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	cmd.ExtraFiles = []*os.File{writer}
	if err := cmd.Start(); err != nil {
		return buildSession{}, err
	}
	go func() { _ = cmd.Wait() }()
	_ = writer.Close()
	var reply startupReply
	if err := json.NewDecoder(reader).Decode(&reply); err != nil {
		if ctx.Err() != nil {
			return buildSession{}, ctx.Err()
		}
		return buildSession{}, fmt.Errorf("build supervisor did not report startup: %w", err)
	}
	if reply.Error != "" {
		return buildSession{}, errors.New(reply.Error)
	}
	return reply.Session, nil
}

// The guard owns the output lock and outlives the build service. As a Linux
// subreaper it can clean up orphaned descendants even if they changed group.
func guardBuild(top, out string, args []string, ready *os.File) error {
	guardContext, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()
	if err := unix.Prctl(unix.PR_SET_CHILD_SUBREAPER, 1, 0, 0, 0); err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Join(out, "meow"), 0755); err != nil {
		return err
	}
	lock, err := os.OpenFile(lockPath(out), os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return err
	}
	defer lock.Close()
	if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		if !errors.Is(err, syscall.EWOULDBLOCK) {
			return fmt.Errorf("lock %s: %w", out, err)
		}
		var active buildSession
		var result buildResult
		if readJSON(sessionPath(out), &active) == nil && os.IsNotExist(readJSON(active.resultPath(), &result)) {
			return fmt.Errorf("%s is locked (recorded supervisor PID %d; log: %s); use meow status, attach, or stop", out, active.GuardPID, active.Log)
		}
		return fmt.Errorf("%s is locked by another build; use meow status or retry after it exits", out)
	}

	dir, err := os.MkdirTemp("/tmp", "meow-control-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(dir)
	now := time.Now()
	var arguments []string
	for _, arg := range args {
		if !strings.Contains(arg, "=") {
			arguments = append(arguments, arg)
		}
	}
	s := buildSession{
		Product: os.Getenv("TARGET_PRODUCT"), Arguments: arguments, Started: now, GuardPID: os.Getpid(),
		Release: os.Getenv("TARGET_RELEASE"), Variant: os.Getenv("TARGET_BUILD_VARIANT"),
		Log:    filepath.Join(out, "meow", "build-"+now.Format("20060102-150405.000000000")+".log"),
		Socket: filepath.Join(dir, "control.sock"),
	}
	listener, err := net.ListenUnix("unix", &net.UnixAddr{Name: s.Socket, Net: "unix"})
	if err != nil {
		return err
	}
	listener.SetUnlinkOnClose(false)
	listenerFile, err := listener.File()
	_ = listener.Close()
	if err != nil {
		return err
	}
	defer listenerFile.Close()

	resultReader, resultWriter, err := os.Pipe()
	if err != nil {
		return err
	}
	defer resultReader.Close()
	defer resultWriter.Close()
	if err := writeJSON(sessionPath(out), s); err != nil {
		return err
	}
	cmd := selfCommand(append([]string{serviceMode, top, out, s.Log}, args...)...)
	cmd.Dir = top
	// The service also holds a duplicate of the lock in case the guard itself
	// is killed. Neither the lock nor the control FDs pass to Soong.
	cmd.ExtraFiles = []*os.File{listenerFile, resultWriter, lock}
	if err := cmd.Start(); err != nil {
		_ = writeJSON(s.resultPath(), buildResult{ExitCode: 1, Error: err.Error(), Elapsed: time.Since(now)})
		return err
	}
	_ = listenerFile.Close()
	_ = resultWriter.Close()
	signalDone := make(chan struct{})
	defer close(signalDone)
	go func() {
		select {
		case <-guardContext.Done():
			_ = cmd.Process.Signal(syscall.SIGTERM)
		case <-signalDone:
		}
	}()
	_ = json.NewEncoder(ready).Encode(startupReply{Session: s})
	_ = ready.Close() // A disconnected launcher must not cancel the build.

	var result buildResult
	decodeErr := json.NewDecoder(resultReader).Decode(&result)
	waitErr := cmd.Wait()
	reapBuildChildren()
	if decodeErr != nil || waitErr != nil {
		result = buildResult{ExitCode: 1, Error: "build service exited unexpectedly", Elapsed: time.Since(now)}
		if waitErr != nil {
			result.Error += ": " + waitErr.Error()
		}
	}
	return writeJSON(s.resultPath(), result)
}

func reapBuildChildren() {
	for {
		pid, err := syscall.Wait4(-1, nil, syscall.WNOHANG, nil)
		if errors.Is(err, syscall.ECHILD) {
			return
		}
		if pid > 0 || errors.Is(err, syscall.EINTR) {
			continue
		}
		// Only signal our own adopted children. Even if one exits after
		// inspection, its PID cannot be reused before this process reaps it.
		entries, _ := os.ReadDir("/proc")
		for _, entry := range entries {
			id, err := strconv.Atoi(entry.Name())
			if err != nil {
				continue
			}
			data, err := os.ReadFile(filepath.Join("/proc", entry.Name(), "stat"))
			if err == nil {
				if parent, ok := processParent(string(data)); ok && parent == os.Getpid() {
					_ = syscall.Kill(id, syscall.SIGKILL)
				}
			}
		}
		time.Sleep(50 * time.Millisecond)
	}
}

func internalFile(fd int, name string) *os.File {
	syscall.CloseOnExec(fd)
	return os.NewFile(uintptr(fd), name)
}

func serveBuild(r *buildRunner, listenerFile, resultFile, lock *os.File) error {
	defer lock.Close()
	defer resultFile.Close()
	defer listenerFile.Close()
	signals, unregister := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer unregister()
	ctx, stop := context.WithCancel(signals)
	defer stop()
	if err := unix.Prctl(unix.PR_SET_CHILD_SUBREAPER, 1, 0, 0, 0); err != nil {
		return err
	}
	listener, err := net.FileListener(listenerFile)
	if err != nil {
		return err
	}
	_ = listenerFile.Close()
	state := newDashboard(nil, r, nil)
	var mu sync.Mutex
	var clients sync.WaitGroup
	accepted := make(chan struct{})
	go func() {
		defer close(accepted)
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			clients.Add(1)
			go func() {
				defer clients.Done()
				defer conn.Close()
				_ = conn.SetDeadline(time.Now().Add(controlWait))
				var request string
				if json.NewDecoder(conn).Decode(&request) != nil {
					return
				}
				if request == "stop" {
					stop()
				}
				mu.Lock()
				snapshot := state.snapshot(request == "attach")
				mu.Unlock()
				snapshot.PID, snapshot.Stopping = r.pid(), ctx.Err() != nil
				_ = json.NewEncoder(conn).Encode(snapshot)
			}()
		}
	}()
	result := r.run(ctx, func(event any) {
		mu.Lock()
		defer mu.Unlock()
		state.now = time.Now()
		switch msg := event.(type) {
		case outputMsg:
			for _, line := range msg {
				state.output(line)
			}
		case statusMsg:
			state.status(msg)
		case parallelismMsg:
			state.jobs = int(msg)
		}
	})
	reapBuildChildren()
	_ = listener.Close()
	<-accepted
	clients.Wait()
	value := toBuildResult(result)
	if err := json.NewEncoder(resultFile).Encode(value); err != nil {
		// The guard is gone. We still own its lock and have reaped our
		// children, so the result can be published directly.
		return writeJSON(buildSession{Log: r.logPath}.resultPath(), value)
	}
	return nil
}

func toBuildResult(result finishedMsg) buildResult {
	value := buildResult{Elapsed: result.elapsed}
	if result.err == nil {
		return value
	}
	value.ExitCode, value.Error = 1, result.err.Error()
	var exit *exec.ExitError
	if errors.Is(result.err, context.Canceled) {
		value.ExitCode = 130
	} else if errors.As(result.err, &exit) && exit.ExitCode() > 0 {
		value.ExitCode = exit.ExitCode()
	}
	return value
}

func (s buildSession) request(ctx context.Context, request string) (buildSnapshot, error) {
	var d net.Dialer
	conn, err := d.DialContext(ctx, "unix", s.Socket)
	if err != nil {
		return buildSnapshot{}, err
	}
	defer conn.Close()
	defer context.AfterFunc(ctx, func() { _ = conn.Close() })()
	_ = conn.SetDeadline(time.Now().Add(controlWait))
	var snapshot buildSnapshot
	if err := json.NewEncoder(conn).Encode(request); err != nil {
		return snapshot, err
	}
	if err := json.NewDecoder(conn).Decode(&snapshot); err != nil {
		return snapshot, err
	}
	if snapshot.Active < 0 || snapshot.Active >= len(snapshot.Tasks) {
		return snapshot, fmt.Errorf("invalid build phase from control socket")
	}
	return snapshot, nil
}
