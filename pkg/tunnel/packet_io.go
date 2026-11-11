package tunnel

import (
	"net"

	"github.com/wencaiwulue/kubevpn/v2/pkg/config"
)

// packetReader converts framed datagrams into independently owned canonical
// packets. Deadline, diagnostics and role-specific policy remain with the caller.
type packetReader struct {
	conn net.Conn
}

func (r *packetReader) read() (*packet, error) {
	packet := newPacketFromPool(config.LargeBufferSize)
	n, err := r.conn.Read(packet.data[datagramHeaderLen:])
	if err != nil {
		packet.release()
		return nil, err
	}
	packet.length = n
	return packet, nil
}
