package daemon

import (
	"net/netip"

	"github.com/anesthetised/lotsman/internal/awgconf"
	"github.com/anesthetised/lotsman/internal/config"
	"github.com/anesthetised/lotsman/internal/store"
)

// Client-side obfuscation: junk before handshakes and a QUIC-looking signature
// packet. These only need to exist on the sending side, so the server does not set them.
var clientJunk = awgconf.Params{
	Jc: 4, Jmin: 40, Jmax: 70,
	I: [5]string{"<b 0xc000000001><r 64><t>"},
}

var everything = netip.MustParsePrefix("0.0.0.0/0")

// ClientConfig renders what a user imports into the Amnezia app for one peer.
func ClientConfig(cfg *config.Config, id Identity, peer store.Peer, mtu int) *awgconf.Config {
	params := id.Params
	params.Jc, params.Jmin, params.Jmax, params.I = clientJunk.Jc, clientJunk.Jmin, clientJunk.Jmax, clientJunk.I
	return &awgconf.Config{
		Interface: awgconf.Interface{
			PrivateKey: peer.PrivateKey,
			Addresses:  []netip.Prefix{netip.PrefixFrom(peer.IP, 32)},
			DNS:        cfg.DNS,
			MTU:        mtu,
			Params:     params,
		},
		Peers: []awgconf.Peer{{
			PublicKey:           id.PrivateKey.Public(),
			Endpoint:            cfg.Endpoint,
			AllowedIPs:          []netip.Prefix{everything},
			PersistentKeepalive: 25,
		}},
	}
}
