package core

import (
	"context"
	"net"
	"time"

	"github.com/wencaiwulue/kubevpn/v2/pkg/config"
	plog "github.com/wencaiwulue/kubevpn/v2/pkg/log"
	"github.com/wencaiwulue/kubevpn/v2/pkg/util"
)

const (
	// MaxSize is the channel buffer capacity for packet queues (inbound, outbound, slots).
	MaxSize = 1000

	// maxConsecutiveTunReadErrors bounds how many back-to-back TUN read errors are tolerated
	// before the device is declared dead. A single transient error (e.g. the wireguard route
	// listener surfacing ENOBUFS once on macOS — the fd stays usable afterwards) must not tear
	// down the data plane, but a genuinely broken device must still be torn down.
	maxConsecutiveTunReadErrors = 10
	// tunReadErrorBackoff paces retries so a persistently failing read cannot hot-spin.
	tunReadErrorBackoff = 100 * time.Millisecond
)

// tunDevice is the single, symmetric TUN engine for both the client and server roles. It runs
// the universal read-tun / write-tun loops; the role-specific routing and goroutines live behind
// the transport strategy (clientTransport dials a conn pool; serverTransport uses RouteHub).
type tunDevice struct {
	tun         net.Conn
	tunInbound  chan *Packet
	tunOutbound chan *Packet
	errChan     chan error
	transport   transport
}

// routines returns every goroutine the device runs: the two universal TUN loops plus the
// transport's role-specific routines. serve() starts and drains them.
func (d *tunDevice) routines() []namedRoutine {
	return append([]namedRoutine{
		{"read-tun", d.readFromTun},
		{"write-tun", d.writeToTun},
	}, d.transport.routines()...)
}

// readFromTun reads IP packets from the TUN and hands each to the transport, which decides how
// it is routed (client: hash to a conn-pool slot or local gvisor; server: enqueue for RouteHub).
func (d *tunDevice) readFromTun(ctx context.Context) {
	d.pumpTun(ctx, d.transport.label(), func(buf []byte, n int, src, dst net.IP) {
		d.transport.routeOutbound(ctx, buf, n, src, dst)
	})
}

// pumpTun reads IP packets from the TUN into pooled buffers (IP data at buf[3:], reserving
// [2-byte datagram header][1-byte type prefix]) and hands each to dispatch. errLabel prefixes
// the read/parse error logs. A parse error drops the packet (buffer returned to the pool) and
// continues. A read error is tolerated: transient/one-off errors are logged and retried (e.g. the
// macOS wireguard route listener surfaces ENOBUFS once but the fd stays usable); only after
// maxConsecutiveTunReadErrors back-to-back failures is the device declared dead (reported via
// errChan, stopping the loop). On a delivered packet the dispatch callback owns buf and must
// return it to config.LPool (directly, or by handing it to a channel whose drainer frees it).
// Both transports share this loop via tunDevice.readFromTun; only their routeOutbound dispatch
// (debug label, framing, routing) differs.
func (d *tunDevice) pumpTun(ctx context.Context, errLabel string, dispatch func(buf []byte, n int, src, dst net.IP)) {
	defer util.HandleCrash()
	consecutiveErrors := 0
	for ctx.Err() == nil {
		buf := config.LPool.Get().([]byte)[:]
		n, err := d.tun.Read(buf[tunReserve:])
		if err != nil {
			config.LPool.Put(buf[:])
			if ctx.Err() != nil {
				return // shutting down: the read error is expected, stay quiet
			}
			consecutiveErrors++
			if consecutiveErrors >= maxConsecutiveTunReadErrors {
				plog.G(ctx).Errorf("%s TUN read failed %d times in a row, giving up: %v", errLabel, consecutiveErrors, err)
				util.SafeWrite(d.errChan, err)
				return
			}
			// Likely transient (e.g. macOS route-listener ENOBUFS surfaced once); the fd usually
			// stays usable, so back off briefly and retry instead of tearing down the data plane.
			plog.G(ctx).Warnf("%s TUN read error (%d/%d), retrying: %v", errLabel, consecutiveErrors, maxConsecutiveTunReadErrors, err)
			time.Sleep(tunReadErrorBackoff)
			continue
		}
		consecutiveErrors = 0
		src, dst, _, parseErr := util.ParseIPFast(buf[tunReserve : tunReserve+n])
		if parseErr != nil {
			plog.G(ctx).Errorf("%s Unknown packet, dropping: %v", errLabel, parseErr)
			config.LPool.Put(buf[:])
			continue
		}
		dispatch(buf, n, src, dst)
	}
}

func (d *tunDevice) writeToTun(ctx context.Context) {
	defer util.HandleCrash()
	defer drainPacketChan(d.tunOutbound)
	for {
		select {
		case packet := <-d.tunOutbound:
			if packet == nil {
				return
			}
			_, err := d.tun.Write(packet.data[tunReserve : datagramHeaderLen+packet.length])
			config.LPool.Put(packet.data[:])
			if err != nil {
				plog.G(ctx).Errorf("[TUN] Failed to write to tun device: %v", err)
				util.SafeWrite(d.errChan, err)
				return
			}
		case <-ctx.Done():
			return
		}
	}
}

func (d *tunDevice) Close() {
	d.tun.Close()
}

// drainPacketChan returns any remaining packet buffers to the pool to prevent leaks
// when a goroutine exits due to context cancellation.
func drainPacketChan(ch <-chan *Packet) {
	for {
		select {
		case pkt := <-ch:
			if pkt == nil {
				return
			}
			config.LPool.Put(pkt.data[:])
		default:
			return
		}
	}
}
