package tunnel

import "net"

// packetReader converts framed datagrams into canonical packets. Its reusable
// owner avoids a per-packet struct allocation on synchronous dispatch paths;
// dispatch must detach the owner before transferring it to an asynchronous queue.
// Deadline, diagnostics and role-specific policy remain with the caller.
type packetReader struct {
	conn   net.Conn
	packet packet
}

func (r *packetReader) read() (*packet, error) {
	r.packet = packet{data: packetBufferFromPool()}
	n, err := r.conn.Read(r.packet.data[datagramHeaderLen:])
	if err != nil {
		r.packet.release()
		return nil, err
	}
	r.packet.length = n
	return &r.packet, nil
}
