package goroslib

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/aler9/goroslib/pkg/apimaster"
	"github.com/aler9/goroslib/pkg/msgs/geometry_msgs"
	"github.com/aler9/goroslib/pkg/prototcp"
)

func TestScoutServiceLookupCancellation(t *testing.T) {
	finished := make(chan struct{})
	release := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.Copy(io.Discard, r.Body)
		select {
		case <-r.Context().Done():
		case <-release:
		}
		close(finished)
	}))
	defer server.Close()
	defer close(release)
	n := &Node{conf: NodeConf{Name: "test", Namespace: "/"}, apiMasterClient: apimaster.NewClient(strings.TrimPrefix(server.URL, "http://"), "test")}
	c := &ServiceClient{conf: ServiceClientConf{Node: n, Name: "/test"}}
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	start := time.Now()
	if err := c.createConn(ctx); err == nil {
		t.Fatal("lookup succeeded unexpectedly")
	}
	if time.Since(start) > time.Second {
		t.Fatal("lookup ignored cancellation")
	}
	select {
	case <-finished:
	case <-time.After(time.Second):
		t.Fatal("HTTP request remained active")
	}
}

func TestScoutServiceResponseCancellation(t *testing.T) {
	s, err := prototcp.NewServer("127.0.0.1:0", net.ParseIP("127.0.0.1"), "")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	conn, err := prototcp.NewClient(fmt.Sprintf("127.0.0.1:%d", s.Port()))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	peer, err := s.Accept()
	if err != nil {
		t.Fatal(err)
	}
	defer peer.Close()
	c := &ServiceClient{conn: conn}
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	start := time.Now()
	if _, err := c.writeMessageReadResponse(ctx, &struct{}{}, &struct{}{}); err == nil {
		t.Fatal("expected timeout")
	}
	if time.Since(start) > time.Second {
		t.Fatal("response ignored cancellation")
	}
}

func TestScoutServiceHandshakeCancellation(t *testing.T) {
	s, err := prototcp.NewServer("127.0.0.1:0", net.ParseIP("127.0.0.1"), "")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	release := make(chan struct{})
	peerDone := make(chan struct{})
	go func() {
		defer close(peerDone)
		peer, err := s.Accept()
		if err != nil {
			return
		}
		defer peer.Close()
		_, _ = peer.ReadHeaderRaw()
		<-release
	}()
	defer func() { close(release); s.Close(); <-peerDone }()
	master := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.Copy(io.Discard, r.Body)
		fmt.Fprintf(w, `<?xml version="1.0"?><methodResponse><params><param><value><array><data><value><int>1</int></value><value><string>ok</string></value><value><string>rosrpc://127.0.0.1:%d</string></value></data></array></value></param></params></methodResponse>`, s.Port())
	}))
	defer master.Close()
	n := &Node{conf: NodeConf{Name: "test", Namespace: "/"}, apiMasterClient: apimaster.NewClient(strings.TrimPrefix(master.URL, "http://"), "test")}
	c := &ServiceClient{conf: ServiceClientConf{Node: n, Name: "/test"}}
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	start := time.Now()
	if err := c.createConn(ctx); err == nil {
		t.Fatal("expected handshake cancellation")
	}
	if time.Since(start) > time.Second {
		t.Fatal("handshake ignored cancellation")
	}
}

func TestScoutWriteSyncWaitsForTransport(t *testing.T) {
	p := &Publisher{ctx: context.Background(), conf: PublisherConf{Msg: &geometry_msgs.Twist{}}, write: make(chan publishWrite)}
	done := make(chan error, 1)
	go func() { done <- p.WriteSync(context.Background(), &geometry_msgs.Twist{}) }()
	req := <-p.write
	select {
	case <-done:
		t.Fatal("returned before transport acknowledgement")
	default:
	}
	req.done <- nil
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := p.WriteSync(ctx, &geometry_msgs.Twist{}); err == nil {
		t.Fatal("ignored cancellation")
	}
}
