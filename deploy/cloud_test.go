package deploy

import (
	"context"
	"net"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestWaitForSSHSucceedsWhenPortOpen(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer ln.Close()
	_, portStr, _ := net.SplitHostPort(ln.Addr().String())
	port, _ := strconv.Atoi(portStr)

	if err := waitForSSH(context.Background(), Target{Host: "127.0.0.1", Port: port}, 2*time.Second); err != nil {
		t.Fatalf("expected success against an open port, got %v", err)
	}
}

func TestWaitForSSHTimesOut(t *testing.T) {
	// Reserve a port then close it so nothing is listening.
	ln, _ := net.Listen("tcp", "127.0.0.1:0")
	_, portStr, _ := net.SplitHostPort(ln.Addr().String())
	ln.Close()
	port, _ := strconv.Atoi(portStr)

	// Shrink the poll cadence so the test is fast.
	origPoll, origDial := sshReadyPoll, sshDialTimeout
	sshReadyPoll, sshDialTimeout = 20*time.Millisecond, 100*time.Millisecond
	defer func() { sshReadyPoll, sshDialTimeout = origPoll, origDial }()

	err := waitForSSH(context.Background(), Target{Host: "127.0.0.1", Port: port}, 150*time.Millisecond)
	if err == nil || !strings.Contains(err.Error(), "timed out waiting for SSH") {
		t.Fatalf("expected timeout error, got %v", err)
	}
}
