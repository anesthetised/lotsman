package daemon

import (
	"context"
	"errors"
	"fmt"
	"sync/atomic"
	"testing"

	"github.com/amnezia-vpn/amneziawg-go/tun"

	"github.com/anesthetised/lotsman/internal/dataplane"
	"github.com/anesthetised/lotsman/internal/dataplane/fake"
)

type closingTUN struct {
	tun.Device
	closed atomic.Bool
}

func (t *closingTUN) Close() error {
	err := t.Device.Close()
	t.closed.Store(true)
	return err
}

type shutdownDataplane struct {
	*fake.Dataplane
	setupErr error
	failures int
	onRetry  func()
}

func (d *shutdownDataplane) Setup(down dataplane.Interface, ups []dataplane.Interface, mtu int) error {
	if err := d.Dataplane.Setup(down, ups, mtu); err != nil {
		return err
	}
	return d.setupErr
}

func (d *shutdownDataplane) Teardown() error {
	if d.failures > 0 {
		d.failures--
		return errors.New("injected teardown failure")
	}
	if d.onRetry != nil {
		d.onRetry()
		d.onRetry = nil
	}
	return d.Dataplane.Teardown()
}

func TestShutdownClosesDevicesAndRetriesCleanup(t *testing.T) {
	for _, partial := range []bool{false, true} {
		for _, failures := range []int{0, 1, 2} {
			t.Run(fmt.Sprintf("partial=%v/failures=%d", partial, failures), func(t *testing.T) {
				h := newHarness(t)
				devices := map[string]*closingTUN{}
				create := h.d.newTUN
				h.d.newTUN = func(name string, mtu int) (tun.Device, error) {
					raw, err := create(name, mtu)
					if err != nil {
						return nil, err
					}
					device := &closingTUN{Device: raw}
					devices[name] = device
					return device, nil
				}
				dp := &shutdownDataplane{Dataplane: h.dp, failures: failures}
				if partial {
					dp.setupErr = errors.New("injected partial setup failure")
				}
				if failures == 1 {
					dp.onRetry = func() {
						if !devices[DownstreamInterface].closed.Load() {
							t.Error("cleanup retried before downstream closed")
						}
						for name, device := range devices {
							if name != DownstreamInterface && device.closed.Load() {
								t.Errorf("upstream %s closed before cleanup retry", name)
							}
						}
					}
				}
				h.d.dp = dp
				t.Cleanup(h.d.stop)
				if partial {
					if err := h.d.Run(context.Background()); !errors.Is(err, dp.setupErr) {
						t.Fatalf("Run: %v", err)
					}
				} else {
					if err := h.d.start(context.Background()); err != nil {
						t.Fatal(err)
					}
					h.d.stop()
				}
				for name, device := range devices {
					if !device.closed.Load() {
						t.Errorf("device %s survived shutdown", name)
					}
				}
				if h.dp.TornDown != (failures < 2) {
					t.Errorf("cleanup completed=%v after %d failures", h.dp.TornDown, failures)
				}
				h.d.stop() // safe to retry after persistent errors and after success
				if !h.dp.TornDown {
					t.Error("subsequent shutdown did not finish cleanup")
				}
			})
		}
	}
}
