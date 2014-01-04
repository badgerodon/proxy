package proxy

import (
	"net"
	"testing"
)

func TestProxyListen(t *testing.T) {
	proxy := newProxy()
	if proxy.listen(0) != nil {
		t.Errorf("expected to do nothing if ports match")
	}

	l1, err := net.Listen("tcp", ":10000")
	if err != nil {
		t.Fatal(err)
	}
	err = proxy.listen(10000)
	if err == nil {
		t.Errorf("expected to receive error on bound port")
	}
	l1.Close()

	proxy.listen(10001)
	proxy.listen(10002)
	l2, err := net.Listen("tcp", ":10001")
	if err != nil {
		t.Errorf("expected existing port to be closed")
	} else {
		l2.Close()
	}

	if proxy.config.Port != 10002 {
		t.Errorf("expected config port to match most recent listen")
	}
	if len(proxy.frontends) != 1 {
		t.Errorf("expected frontends to contain only the most recent frontend")
	}
}

func TestGetTLSConfig(t *testing.T) {

}
