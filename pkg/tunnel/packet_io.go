package tunnel

import (
	"net"

	"gvisor.dev/gvisor/pkg/buffer"

	"github.com/wencaiwulue/kubevpn/v2/pkg/config"
)

// packetReader converts framed datagrams into independently owned packets.
// framing.Conn supplies exact ingress Views for zero-copy gVisor injection;
// other net.Conn implementations use the canonical pooled buffer layout.
// Deadline, diagnostics and role-specific policy remain with the caller.
type packetReader struct {
	conn net.Conn
}

func (r *packetReader) read() (*packet, error) {
	if conn, ok := r.conn.(interface{ ReadView() (*buffer.View, error) }); ok {
		view, err := conn.ReadView()
		if err != nil {
			return nil, err
		}
		return newViewPacket(view), nil
	}
	packet := newPacketFromPool(config.LargeBufferSize)
	n, err := r.conn.Read(packet.data[datagramHeaderLen:])
	if err != nil {
		packet.release()
		return nil, err
	}
	packet.length = n
	return packet, nil
}
