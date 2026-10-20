//go:build integration

package stacktest

import (
	"fmt"
	"io"
	"net"
	"os"
	"strings"
	"testing"
)

// TestMain starts a loopback forward to the stack's Postgres when
// ZS_TEST_POSTGRES_FORWARD is set to "<loopback addr>=<host:port>", such as
// 127.0.0.1:55432=postgres:5432. The Postgres harness accepts loopback
// servers only, and the push notification test runs inside Kill Bill's
// network namespace, where the stack's Postgres is reached by its service
// name. See docs/development.md.
func TestMain(m *testing.M) {
	if spec := os.Getenv("ZS_TEST_POSTGRES_FORWARD"); spec != "" {
		listen, target, ok := strings.Cut(spec, "=")
		host, _, err := net.SplitHostPort(listen)
		ip := net.ParseIP(host)
		if !ok || err != nil || ip == nil || !ip.IsLoopback() || target == "" {
			fmt.Fprintln(os.Stderr, "ZS_TEST_POSTGRES_FORWARD must be <loopback addr>=<host:port>")
			os.Exit(2)
		}
		l, err := net.Listen("tcp", listen)
		if err != nil {
			fmt.Fprintln(os.Stderr, "ZS_TEST_POSTGRES_FORWARD:", err)
			os.Exit(2)
		}
		go forward(l, target)
	}
	os.Exit(m.Run())
}

func forward(l net.Listener, target string) {
	for {
		c, err := l.Accept()
		if err != nil {
			return
		}
		go func(c net.Conn) {
			defer c.Close()
			d, err := net.Dial("tcp", target)
			if err != nil {
				return
			}
			defer d.Close()
			go func() { _, _ = io.Copy(d, c) }()
			_, _ = io.Copy(c, d)
		}(c)
	}
}
