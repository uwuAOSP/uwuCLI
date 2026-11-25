/*
 * SPDX-FileCopyrightText: The uwuAOSP Project
 * SPDX-License-Identifier: Apache-2.0
 */

package main

import (
	"bufio"
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"sync"
)

// Wire format belongs to Soong. The dashboard has no scheduler dependency.
type statusEvent struct {
	Type                  string            `json:"type"`
	ID                    uint64            `json:"id"`
	Description           string            `json:"description"`
	StartedUnixNano       int64             `json:"started_unix_nano"`
	Total                 int               `json:"total"`
	Completed             int               `json:"completed"`
	EstimatedTimeUnixNano int64             `json:"estimated_time_unix_nano"`
	BuildConfig           map[string]string `json:"build_config"`
	Targets               []string          `json:"targets"`
}

type statusMsg struct {
	connection uint64
	event      statusEvent
}

type statusServer struct {
	dir, path   string
	listener    *net.UnixListener
	mu          sync.Mutex
	connections map[*net.UnixConn]struct{}
	closing     bool
	acceptDone  chan struct{}
	workers     sync.WaitGroup
}

func listenStatus(send func(any)) (*statusServer, error) {
	dir, err := os.MkdirTemp("", "meow-")
	if err != nil {
		return nil, err
	}
	s := &statusServer{dir: dir, path: filepath.Join(dir, "status.sock"), connections: make(map[*net.UnixConn]struct{}), acceptDone: make(chan struct{})}
	s.listener, err = net.ListenUnix("unix", &net.UnixAddr{Name: s.path, Net: "unix"})
	if err != nil {
		_ = os.RemoveAll(dir)
		return nil, err
	}
	go func() {
		defer close(s.acceptDone)
		var sequence uint64
		for {
			conn, err := s.listener.AcceptUnix()
			if err != nil {
				return
			}
			s.mu.Lock()
			if s.closing {
				s.mu.Unlock()
				_ = conn.Close()
				return
			}
			s.connections[conn] = struct{}{}
			s.workers.Add(1)
			s.mu.Unlock()
			sequence++
			go s.read(conn, sequence, send)
		}
	}()
	return s, nil
}

func (s *statusServer) read(conn *net.UnixConn, id uint64, send func(any)) {
	defer s.workers.Done()
	defer func() { _ = conn.Close(); s.mu.Lock(); delete(s.connections, conn); s.mu.Unlock() }()
	scan := bufio.NewScanner(conn)
	scan.Buffer(make([]byte, 4096), 1<<20)
	for scan.Scan() {
		var event statusEvent
		if json.Unmarshal(scan.Bytes(), &event) == nil {
			send(statusMsg{id, event})
		}
	}
	send(statusMsg{id, statusEvent{Type: "disconnect"}})
}

func (s *statusServer) close() {
	_ = s.listener.Close()
	s.mu.Lock()
	s.closing = true
	for conn := range s.connections {
		_ = conn.Close()
	}
	s.mu.Unlock()
	<-s.acceptDone
	s.workers.Wait()
	_ = os.RemoveAll(s.dir)
}
