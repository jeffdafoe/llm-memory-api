// Package watchdog sends systemd's watchdog heartbeat. The unit sets
// WatchdogSec; when the pings stop arriving systemd kills and restarts the
// service.
//
// The Node server needed this because one blocked event loop froze every
// request. Go has no single loop to block, so here the ping mostly keeps the
// existing unit contract: the unit is shared in shape with the Node one, and
// a unit with WatchdogSec kills a service that never pings. The ping runs on
// its own goroutine, so a total stall of the process (deadlock in the
// runtime, the process stopped) still stops it.
package watchdog

import (
	"net"
	"os"
	"strconv"
	"time"

	"github.com/jeffdafoe/llm-memory-api/go/server/internal/logger"
)

const writeTimeout = 5 * time.Second

// Start begins the heartbeat when systemd asked for one (NOTIFY_SOCKET and a
// positive WATCHDOG_USEC are set). Otherwise, as in local runs, it does
// nothing.
func Start(log *logger.Logger) {
	socket := os.Getenv("NOTIFY_SOCKET")
	usec, err := strconv.ParseInt(os.Getenv("WATCHDOG_USEC"), 10, 64)
	if socket == "" || err != nil || usec <= 0 {
		return
	}
	// Half the watchdog interval, per systemd's guidance, so one missed beat
	// does not trip it.
	interval := time.Duration(usec) * time.Microsecond / 2
	if interval < time.Second {
		interval = time.Second
	}

	ping := func() {
		if err := notify(socket, "WATCHDOG=1"); err != nil {
			log.LogError("watchdog", "systemd-notify failed", logger.ErrorOpts{Message: err.Error()})
		}
	}
	ping()
	go func() {
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for range ticker.C {
			ping()
		}
	}()
	log.Log("watchdog", "systemd-heartbeat-started", logger.Fields{
		{Key: "watchdogSec", Value: float64(usec) / 1e6},
		{Key: "heartbeatMs", Value: interval.Milliseconds()},
	})
}

// notify writes one sd_notify datagram. A socket name starting with "@" is in
// the abstract namespace, which Go spells with a leading NUL byte.
func notify(socket, state string) error {
	name := socket
	if name[0] == '@' {
		name = "\x00" + name[1:]
	}
	conn, err := net.DialUnix("unixgram", nil, &net.UnixAddr{Name: name, Net: "unixgram"})
	if err != nil {
		return err
	}
	defer conn.Close()
	conn.SetWriteDeadline(time.Now().Add(writeTimeout))
	_, err = conn.Write([]byte(state))
	return err
}
