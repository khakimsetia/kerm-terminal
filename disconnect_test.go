package main

import (
	"errors"
	"testing"
)

func TestDisconnectReason(t *testing.T) {
	if info := disconnectReason(nil); !info.clean {
		t.Fatalf("nil must be clean, got %+v", info)
	}
	if info := disconnectReason(errors.New("use of closed network connection")); !info.clean {
		t.Fatalf("local close must be clean, got %+v", info)
	}
	cases := []struct{ err, want string }{
		{"read tcp: connection reset by peer", "Connection Reset"},
		{"write: broken pipe", "Connection Reset"},
		{"i/o timeout", "Connection Timeout"},
		{"ssh: disconnect, reason 11", "Disconnected by Server"},
		{"EOF", "Connection Closed by Server"},
		{"something unknown", "Connection Lost"},
	}
	for _, c := range cases {
		info := disconnectReason(errors.New(c.err))
		if info.clean || info.reason != c.want {
			t.Fatalf("err %q: got %+v, want reason %q", c.err, info, c.want)
		}
		if info.detail != c.err {
			t.Fatalf("detail must be the original error: got %q", info.detail)
		}
	}
}

func TestConnectFailureReason(t *testing.T) {
	cases := []struct {
		err         string
		notify      bool
		wantTitle   string
		wantDetail  string
	}{
		{"SSH failed: dial tcp 1.2.3.4:22: i/o timeout", true, "Connection Timeout", "i/o timeout"},
		{"dial tcp: connection refused", true, "Connection Refused", "connection refused"},
		{"dial tcp: lookup example.invalid: no such host", true, "Host Not Found", "no such host"},
		{"ssh: handshake failed: ssh: unable to authenticate, attempted methods [none password], no supported methods remain", true, "Authentication Failed", "unable to authenticate"},
		{"host key not confirmed — connection cancelled", false, "Host Key Not Confirmed", ""},
		{"host key changed — connection cancelled", false, "Host Key Changed", ""},
		{"something else went wrong", true, "Connection Failed", "something else went wrong"},
	}
	for _, c := range cases {
		title, detail, notify := connectFailureReason(errors.New(c.err))
		if notify != c.notify {
			t.Fatalf("err %q: notify = %v, want %v", c.err, notify, c.notify)
		}
		if title != c.wantTitle {
			t.Fatalf("err %q: title = %q, want %q", c.err, title, c.wantTitle)
		}
		if detail != c.wantDetail {
			t.Fatalf("err %q: detail = %q, want %q", c.err, detail, c.wantDetail)
		}
	}
}

func TestDisconnectNotice(t *testing.T) {
	got := disconnectNotice("root@103.195.65.133", "Connection Timeout", "2026-10-29 20:44:00")
	want := "Disconnected from root@103.195.65.133\r\nNetwork error: Connection Timeout\r\nTime: 2026-10-29 20:44:00"
	if got != want {
		t.Fatalf("notice = %q, want %q", got, want)
	}
	if got := disconnectLine("root@103.195.65.133"); got != "Disconnected from root@103.195.65.133" {
		t.Fatalf("line = %q", got)
	}
}
