package helper

import (
	"log/slog"
	"net/netip"
	"os"
	"os/exec"
	"strings"
	"testing"

	"github.com/amnezia-vpn/amneziawg-go/v3/tun/netstack"

	"github.com/anesthetised/lotsman/internal/proxy"
)

const vlessID = "b831381d-6324-4d53-ad4f-8cda48b30811"

// A VLESS upstream runs in a helper like any proxy upstream: the child
// starts its Xray instance from the link, and Xray's log, which the child
// sends to its own log, reaches the daemon's.
func TestRunnerVLESS(t *testing.T) {
	r := Runner{Command: func() *exec.Cmd {
		cmd := exec.Command(os.Args[0])
		cmd.Env = append(os.Environ(), testHelperEnv+"=serve")
		return cmd
	}}
	dev, _, err := netstack.CreateNetTUN([]netip.Addr{netip.MustParseAddr("10.0.0.1")}, nil, 1500)
	if err != nil {
		t.Fatal(err)
	}
	f, err := os.Open(os.DevNull)
	if err != nil {
		t.Fatal(err)
	}
	logs := &syncBuffer{}
	link := "vless://" + vlessID + "@192.0.2.1:443?security=reality&pbk=wsxdeEB2O7Mqa1ny8bxMHOMyTzvWvvVQXqhNBNPMn2E&sni=www.example.com&flow=xtls-rprx-vision"
	spec := proxy.Spec{Key: link, Server: netip.MustParseAddrPort("192.0.2.1:443"), MaxFlows: 3}
	l, err := r.Start(fileTUN{dev, f}, spec, slog.New(slog.NewTextHandler(logs, nil)).With("upstream", "vl"), func(error) {})
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	if err := l.Up(); err != nil {
		t.Fatal(err)
	}
	if st, err := l.Stats(); err != nil || st.MaxFlows != 3 {
		t.Errorf("stats = %+v, %v", st, err)
	}
	// A moved server gets a new Xray instance in the helper.
	if err := l.SetServer(link, netip.MustParseAddrPort("192.0.2.2:443")); err != nil {
		t.Fatal(err)
	}
	// The child's Xray has the gRPC transport.
	if err := l.SetServer(strings.Replace(link, "flow=xtls-rprx-vision", "type=grpc&serviceName=x", 1), netip.MustParseAddrPort("192.0.2.2:443")); err != nil {
		t.Fatal(err)
	}
	if err := l.SetServer(strings.Replace(link, "flow=xtls-rprx-vision", "flow=none", 1), netip.MustParseAddrPort("192.0.2.2:443")); err == nil {
		t.Error("the helper took a link it cannot use")
	}
	waitLog(t, logs, "component=xray")
	if !strings.Contains(logs.String(), "upstream=vl") {
		t.Errorf("xray's log lacks the upstream:\n%s", logs)
	}
	l.Close()
	if strings.Contains(logs.String(), vlessID) {
		t.Error("the log shows the user ID")
	}
}
