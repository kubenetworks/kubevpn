package util

import (
	"encoding/binary"
	"net"
	"testing"
)

func TestIsIPv4_EmptySlice(t *testing.T) {
	// Must not panic on an empty slice (callers may pass buf[1:read] with read==1).
	if IsIPv4([]byte{}) {
		t.Error("expected false for empty slice")
	}
}

func TestIsIPv6_EmptySlice(t *testing.T) {
	if IsIPv6([]byte{}) {
		t.Error("expected false for empty slice")
	}
}

// buildIPv4Header constructs a minimal 20-byte IPv4 header with the given src and dst IPs.
// Protocol is set to TCP (6).
func buildIPv4Header(src, dst net.IP) []byte {
	hdr := make([]byte, 20)
	hdr[0] = 0x45                            // version 4, IHL 5 (20 bytes)
	hdr[1] = 0x00                            // DSCP/ECN
	binary.BigEndian.PutUint16(hdr[2:4], 20) // total length
	// identification, flags, fragment offset left as zero
	hdr[8] = 64 // TTL
	hdr[9] = 6  // protocol: TCP
	hdr[10] = 0 // checksum (left zero for test)
	hdr[11] = 0 // checksum
	copy(hdr[12:16], src.To4())
	copy(hdr[16:20], dst.To4())
	return hdr
}

// buildIPv6Header constructs a minimal 40-byte IPv6 header with the given src and dst IPs.
// NextHeader is set to TCP (6).
func buildIPv6Header(src, dst net.IP) []byte {
	hdr := make([]byte, 40)
	hdr[0] = 0x60                           // version 6, traffic class high nibble
	hdr[1] = 0x00                           // traffic class low + flow label
	hdr[2] = 0x00                           // flow label
	hdr[3] = 0x00                           // flow label
	binary.BigEndian.PutUint16(hdr[4:6], 0) // payload length
	hdr[6] = 6                              // next header: TCP
	hdr[7] = 64                             // hop limit
	copy(hdr[8:24], src.To16())
	copy(hdr[24:40], dst.To16())
	return hdr
}

func TestParseIP(t *testing.T) {
	tests := []struct {
		name         string
		packet       []byte
		wantSrc      net.IP
		wantDst      net.IP
		wantProtocol int
		wantErr      bool
	}{
		{
			name:         "valid IPv4 packet",
			packet:       buildIPv4Header(net.ParseIP("192.168.1.100").To4(), net.ParseIP("10.0.0.1").To4()),
			wantSrc:      net.ParseIP("192.168.1.100"),
			wantDst:      net.ParseIP("10.0.0.1"),
			wantProtocol: 6, // TCP
			wantErr:      false,
		},
		{
			name:         "valid IPv6 packet",
			packet:       buildIPv6Header(net.ParseIP("fd00::1"), net.ParseIP("fd00::2")),
			wantSrc:      net.ParseIP("fd00::1"),
			wantDst:      net.ParseIP("fd00::2"),
			wantProtocol: 6, // TCP
			wantErr:      false,
		},
		{
			name:         "IPv4 packet too short",
			packet:       []byte{0x45, 0x00, 0x00},
			wantSrc:      nil,
			wantDst:      nil,
			wantProtocol: -1,
			wantErr:      true,
		},
		{
			name:         "IPv6 packet too short",
			packet:       []byte{0x60, 0x00, 0x00},
			wantSrc:      nil,
			wantDst:      nil,
			wantProtocol: -1,
			wantErr:      true,
		},
		{
			name:         "non-IP packet version 3",
			packet:       []byte{0x30, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00},
			wantSrc:      nil,
			wantDst:      nil,
			wantProtocol: -1,
			wantErr:      true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			src, dst, protocol, err := ParseIP(tt.packet)
			if (err != nil) != tt.wantErr {
				t.Errorf("ParseIP() error = %v, wantErr %v", err, tt.wantErr)
				return
			}
			if tt.wantErr {
				if protocol != tt.wantProtocol {
					t.Errorf("ParseIP() protocol = %v, want %v", protocol, tt.wantProtocol)
				}
				return
			}
			if !src.Equal(tt.wantSrc) {
				t.Errorf("ParseIP() src = %v, want %v", src, tt.wantSrc)
			}
			if !dst.Equal(tt.wantDst) {
				t.Errorf("ParseIP() dst = %v, want %v", dst, tt.wantDst)
			}
			if protocol != tt.wantProtocol {
				t.Errorf("ParseIP() protocol = %v, want %v", protocol, tt.wantProtocol)
			}
		})
	}
}
