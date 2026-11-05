package tunnel

import (
	"context"
	"net"
)

func (s *connSlot) readFromConn(ctx context.Context, conn net.Conn, errChan chan error) {
	(&connAttempt{connSlot: s}).readGeneration(ctx, conn, errChan, s.stats.Generation())
}

func (s *connSlot) writeToConn(ctx context.Context, rawConn net.Conn, errChan chan error) {
	(&connAttempt{connSlot: s}).writeGeneration(ctx, rawConn, errChan, s.stats.Generation())
}

func (s *connSlot) readGeneration(ctx context.Context, conn net.Conn, errs chan error, generation uint64) {
	(&connAttempt{connSlot: s}).readGeneration(ctx, conn, errs, generation)
}
func (s *connSlot) writeGeneration(ctx context.Context, conn net.Conn, errs chan error, generation uint64) {
	(&connAttempt{connSlot: s}).writeGeneration(ctx, conn, errs, generation)
}
func (s *connSlot) writePayload(conn net.Conn, payload []byte, generation uint64, probe bool) (int, error) {
	return (&connAttempt{connSlot: s}).writePayload(conn, payload, generation, probe)
}
