// Package tunnel wraps an amneziawg-go device with the few operations Lotsman needs.
package tunnel

import (
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"time"

	"github.com/amnezia-vpn/amneziawg-go/conn"
	"github.com/amnezia-vpn/amneziawg-go/device"
	"github.com/amnezia-vpn/amneziawg-go/tun"

	"github.com/anesthetised/lotsman/internal/awgconf"
)

type Device struct {
	name string
	dev  *device.Device
}

// New starts a device on t. The caller owns t's creation (a real TUN on Linux,
// a netstack one in tests) so this package stays portable.
func New(name string, t tun.Device, log *slog.Logger) *Device {
	log = log.With("device", name)
	logger := &device.Logger{
		Verbosef: func(format string, args ...any) { log.Debug(fmt.Sprintf(format, args...)) },
		Errorf:   func(format string, args ...any) { log.Error(fmt.Sprintf(format, args...)) },
	}
	return &Device{name: name, dev: device.NewDevice(t, conn.NewDefaultBind(), logger)}
}

// CreateTUN opens a kernel TUN interface with the given name and MTU.
func CreateTUN(name string, mtu int) (tun.Device, error) {
	return tun.CreateTUN(name, mtu)
}

func (d *Device) Name() string { return d.name }

// Configure applies UAPI lines (see awgconf.Config.UAPI and awgconf.Peer.UAPI).
func (d *Device) Configure(uapi string) error {
	if err := d.dev.IpcSet(uapi); err != nil {
		return fmt.Errorf("%s: %w", d.name, err)
	}
	return nil
}

func (d *Device) AddPeer(p awgconf.Peer) error { return d.Configure(p.UAPI()) }

func (d *Device) RemovePeer(pub awgconf.Key) error {
	return d.Configure("public_key=" + pub.Hex() + "\nremove=true\n")
}

func (d *Device) Up() error {
	if err := d.dev.Up(); err != nil {
		return fmt.Errorf("%s: %w", d.name, err)
	}
	return nil
}

func (d *Device) Close() { d.dev.Close() }

type PeerStatus struct {
	PublicKey     awgconf.Key
	Endpoint      string
	LastHandshake time.Time // zero if never
	RxBytes       uint64
	TxBytes       uint64
}

// Peers reports the runtime state of every peer on the device.
func (d *Device) Peers() ([]PeerStatus, error) {
	raw, err := d.dev.IpcGet()
	if err != nil {
		return nil, fmt.Errorf("%s: %w", d.name, err)
	}
	return parsePeers(raw)
}

// parsePeers reads the UAPI "get" format: device keys first, then a block per
// peer starting with public_key.
func parsePeers(raw string) ([]PeerStatus, error) {
	var peers []PeerStatus
	var cur *PeerStatus
	var sec, nsec int64
	for _, line := range strings.Split(raw, "\n") {
		key, value, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		switch key {
		case "public_key":
			b, err := awgconf.ParseHexKey(value)
			if err != nil {
				return nil, err
			}
			peers = append(peers, PeerStatus{PublicKey: b})
			cur = &peers[len(peers)-1]
			sec, nsec = 0, 0
		case "endpoint":
			cur.Endpoint = value
		case "last_handshake_time_sec":
			sec, _ = strconv.ParseInt(value, 10, 64)
		case "last_handshake_time_nsec":
			nsec, _ = strconv.ParseInt(value, 10, 64)
			if sec != 0 {
				cur.LastHandshake = time.Unix(sec, nsec)
			}
		case "rx_bytes":
			cur.RxBytes, _ = strconv.ParseUint(value, 10, 64)
		case "tx_bytes":
			cur.TxBytes, _ = strconv.ParseUint(value, 10, 64)
		}
	}
	return peers, nil
}
