package core

import (
	"context"
	"net"
	"time"

	"github.com/wencaiwulue/kubevpn/v2/pkg/config"
	plog "github.com/wencaiwulue/kubevpn/v2/pkg/log"
	netutil "github.com/wencaiwulue/kubevpn/v2/pkg/util/netutil"
)

const (
	// MaxSize is the channel buffer capacity for packet queues (inbound, outbound, slots).
	// Each queued *Packet pins a config.LargeBufferSize (64 KiB) pooled buffer, so a full
	// channel's worst case is MaxSize × 64 KiB — and the data plane has 7+ such channels,
	// which multiplies straight into client RSS. 256 keeps a single channel's worst case at
	// 16 MiB (see TestPacketChannelWorstCaseMemory) while still absorbing bursts; an
	// over-deep queue mainly adds worst-case memory and head-of-line latency on the shared
	// tunnel rather than throughput.
	MaxSize = 256

	// maxConsecutiveTunReadErrors bounds how many back-to-back TUN read errors are tolerated
	// before the device is declared dead. A single transient error (e.g. the wireguard route
	// listener surfacing ENOBUFS once on macOS — the fd stays usable afterwards) must not tear
	// down the data plane, but a genuinely broken device must still be torn down.
	maxConsecutiveTunReadErrors = 10
	// tunReadErrorBackoff paces retries so a persistently failing read cannot hot-spin.
	tunReadErrorBackoff = 100 * time.Millisecond
)

// batchTUN is implemented by TUN connections that can read multiple IP packets per syscall
// (wireguard-go offload/GRO). pumpTun uses it for the segment-aware read path — one syscall
// yields up to BatchSize() packets, each dispatched individually — and falls back to plain
// net.Conn single-packet reads when a device does not implement it (e.g. net.Pipe in tests).
type batchTUN interface {
	BatchSize() int
	// ReadPackets reads up to len(bufs) IP packets, writing packet i into bufs[i][off:] and
	// its length into sizes[i]; it returns the number of packets placed into bufs.
	ReadPackets(bufs [][]byte, sizes []int, off int) (int, error)
}

// tunDevice is the single, symmetric TUN engine for both the client and server roles. It runs
// the universal read-tun / write-tun loops; the role-specific routing and goroutines live behind
// the transport strategy (clientTransport dials a conn pool; serverTransport uses RouteHub).
type tunDevice struct {
	tun net.Conn
	// tunName is the OS interface name of tun, resolved ONCE when the device is created (see
	// tunHandler.Handle). Everything that needs the device's current addresses looks them up by
	// name (netutil.GetTunDeviceIP → net.InterfaceByName), never by re-scanning the whole
	// interface table: a scan can fail for reasons unrelated to this device (a transient or
	// misbehaving unrelated NIC), and callers on the reconnect path silently degraded to "no
	// addresses" when it did — which cost the server its route for this client and black-holed
	// every heartbeat echo reply. Empty in tests that drive a device without a real TUN.
	tunName string
	// addrsFn overrides how the device's addresses are resolved. Production leaves it nil (the
	// name-based lookup); only tests that drive a device without a real TUN set it, mirroring
	// clientTransport.poolSize.
	addrsFn     func() (v4, v6, dockerV4 net.IP)
	tunInbound  chan *Packet
	tunOutbound chan *Packet
	errChan     chan error
	transport   transport
}

// newTunDevice constructs a tunDevice bound to tun, resolving the device's OS interface name once
// (netutil.GetTunDeviceByConn) and storing it as tunName — the single source of truth every later
// address lookup uses (see the tunName field). Both the production handler (tunHandler.Handle) and
// the real-TUN integration tests build their devices through here so neither can drift into leaving
// tunName empty: an empty name makes addrs() return all-nil, which silently disables route
// announcement and heartbeats and black-holes the tunnel — the exact regression this guards against.
func newTunDevice(tun net.Conn, errChan chan error) (*tunDevice, error) {
	tunIfi, err := netutil.GetTunDeviceByConn(tun)
	if err != nil {
		return nil, err
	}
	return &tunDevice{
		tun:         tun,
		tunName:     tunIfi.Name,
		tunInbound:  make(chan *Packet, MaxSize),
		tunOutbound: make(chan *Packet, MaxSize),
		errChan:     errChan,
	}, nil
}

// addrs returns the device's current IPv4, IPv6 and Docker-IPv4 TUN addresses, or nils when the
// device has none / cannot be inspected. It is the single seam through which the data plane learns
// its own addresses, and it is deliberately re-read on every call rather than cached: ChangeTunIP
// can reassign them at runtime. Errors collapse to nil addresses — callers must treat that as
// "unknown right now" and retry, never as a permanent condition.
func (d *tunDevice) addrs() (v4, v6, dockerV4 net.IP) {
	if d.addrsFn != nil {
		return d.addrsFn()
	}
	v4, v6, dockerV4, _ = netutil.GetTunDeviceIP(d.tunName)
	return v4, v6, dockerV4
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
	if bt, ok := d.tun.(batchTUN); ok {
		d.pumpTunBatch(ctx, errLabel, bt, dispatch)
		return
	}
	d.pumpTunSingle(ctx, errLabel, dispatch)
}

// pumpTunSingle is the single-packet read loop used for plain net.Conn TUN devices (e.g.
// net.Pipe in tests). Real TUN devices take the batched pumpTunBatch path.
func (d *tunDevice) pumpTunSingle(ctx context.Context, errLabel string, dispatch func(buf []byte, n int, src, dst net.IP)) {
	defer netutil.HandleCrash()
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
				netutil.SafeWrite(d.errChan, err)
				return
			}
			// Likely transient (e.g. macOS route-listener ENOBUFS surfaced once); the fd usually
			// stays usable, so back off briefly and retry instead of tearing down the data plane.
			plog.G(ctx).Warnf("%s TUN read error (%d/%d), retrying: %v", errLabel, consecutiveErrors, maxConsecutiveTunReadErrors, err)
			time.Sleep(tunReadErrorBackoff)
			continue
		}
		consecutiveErrors = 0
		src, dst, _, parseErr := netutil.ParseIPFast(buf[tunReserve : tunReserve+n])
		if parseErr != nil {
			plog.G(ctx).Errorf("%s Unknown packet, dropping: %v", errLabel, parseErr)
			config.LPool.Put(buf[:])
			continue
		}
		dispatch(buf, n, src, dst)
	}
}

// pumpTunBatch is the segment-aware read loop for TUN devices that support batched IO
// (wireguard-go offload/GRO). A single syscall yields up to BatchSize() IP packets —
// including GRO-coalesced ones — and each is parsed and dispatched individually into the
// canonical [2 len][1 type][IP] layout (IP at buf[tunReserve:]). Buffers handed to dispatch
// are owned by the consumer; unused ones from a short batch are returned to the pool here.
func (d *tunDevice) pumpTunBatch(ctx context.Context, errLabel string, bt batchTUN, dispatch func(buf []byte, n int, src, dst net.IP)) {
	defer netutil.HandleCrash()
	bs := bt.BatchSize()
	if bs < 1 {
		bs = 1
	}
	bufs := make([][]byte, bs)
	sizes := make([]int, bs)
	consecutiveErrors := 0
	for ctx.Err() == nil {
		for i := range bufs {
			bufs[i] = config.LPool.Get().([]byte)[:]
		}
		m, err := bt.ReadPackets(bufs, sizes, tunReserve)
		if err != nil {
			for i := range bufs {
				config.LPool.Put(bufs[i][:])
			}
			if ctx.Err() != nil {
				return // shutting down: the read error is expected, stay quiet
			}
			consecutiveErrors++
			if consecutiveErrors >= maxConsecutiveTunReadErrors {
				plog.G(ctx).Errorf("%s TUN read failed %d times in a row, giving up: %v", errLabel, consecutiveErrors, err)
				netutil.SafeWrite(d.errChan, err)
				return
			}
			plog.G(ctx).Warnf("%s TUN read error (%d/%d), retrying: %v", errLabel, consecutiveErrors, maxConsecutiveTunReadErrors, err)
			time.Sleep(tunReadErrorBackoff)
			continue
		}
		consecutiveErrors = 0
		for i := 0; i < m; i++ {
			buf := bufs[i]
			size := sizes[i]
			src, dst, _, parseErr := netutil.ParseIPFast(buf[tunReserve : tunReserve+size])
			if parseErr != nil {
				plog.G(ctx).Errorf("%s Unknown packet, dropping: %v", errLabel, parseErr)
				config.LPool.Put(buf[:])
				continue
			}
			dispatch(buf, size, src, dst)
		}
		for i := m; i < bs; i++ {
			config.LPool.Put(bufs[i][:])
		}
	}
}

func (d *tunDevice) writeToTun(ctx context.Context) {
	defer netutil.HandleCrash()
	defer drainPacketChan(d.tunOutbound)
	for {
		select {
		case packet := <-d.tunOutbound:
			if packet == nil {
				return
			}
			_, err := d.tun.Write(packet.data[tunReserve : datagramHeaderLen+packet.length])
			packet.release()
			if err != nil {
				plog.G(ctx).Errorf("[TUN] Failed to write to tun device: %v", err)
				netutil.SafeWrite(d.errChan, err)
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
			pkt.release()
		default:
			return
		}
	}
}
