//go:build linux

package watchdog

import (
	"net"
	"path/filepath"
	"testing"
	"time"
)

func TestNotifySendsDatagram(t *testing.T) {
	path := filepath.Join(t.TempDir(), "notify.sock")
	conn, err := net.ListenUnixgram("unixgram", &net.UnixAddr{Name: path, Net: "unixgram"})
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()

	if err := notify(path, "WATCHDOG=1"); err != nil {
		t.Fatal(err)
	}
	conn.SetReadDeadline(time.Now().Add(time.Second))
	buf := make([]byte, 64)
	n, err := conn.Read(buf)
	if err != nil || string(buf[:n]) != "WATCHDOG=1" {
		t.Fatalf("read %q, %v", buf[:n], err)
	}
}
