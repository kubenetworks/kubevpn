package util

import (
	"context"
	"crypto/tls"
	"io"
	"net"
	"testing"

	"github.com/wencaiwulue/kubevpn/v2/pkg/config"
)

func TestTLS(t *testing.T) {
	ns := "kubevpn"
	crtBytes, keyBytes, alternateDNS, err := GenTLSCert(context.Background(), GetTLSHost(ns))
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv(config.TLSCertKey, string(crtBytes))
	t.Setenv(config.TLSPrivateKeyKey, string(keyBytes))
	t.Setenv(config.TLSServerName, string(alternateDNS))

	listen, err := net.Listen("tcp", ":9090")
	if err != nil {
		t.Fatal(err)
	}
	tlsServerConfig, err := GetTlsServerConfig(nil)
	if err != nil {
		t.Fatal(err)
	}
	listener := tls.NewListener(listen, tlsServerConfig)
	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				t.Error(err)
				return
			}
			go func(conn net.Conn) {
				bytes := make([]byte, 1024)
				all, err2 := conn.Read(bytes)
				if err2 != nil {
					t.Error(err2)
					return
				}
				defer conn.Close()
				t.Log(string(bytes[:all]))
				io.WriteString(conn, "hello client")
			}(conn)
		}
	}()
	dial, err := net.Dial("tcp", ":9090")
	if err != nil {
		t.Fatal(err)
	}

	clientConfig, err := GetTlsClientConfig(nil)
	if err != nil {
		t.Fatal(err)
	}
	client := tls.Client(dial, clientConfig)
	client.Write([]byte("hi server"))
	all, err := io.ReadAll(client)
	if err != nil {
		t.Fatal(err)
	}
	t.Log(string(all))
}

func TestDecode(t *testing.T) {
	s := "LS0tLS1CRUdJTiBDRVJUSUZJQ0FURS0tLS0tCk1JSURwRENDQW95Z0F3SUJBZ0lVTXl1TzA0Mkx4UjRnMHFybTVtbk00WkozNUdRd0RRWUpLb1pJaHZjTkFRRUwKQlFBd0xqRXNNQ29HQTFVRUF3d2phM1ZpWlhad2JpMTBjbUZtWm1sakxXMWhibUZuWlhJdWEzVmlaWFp3Ymk1egpkbU13SUJjTk1qVXdOREEyTVRRd056UXlXaGdQTWpFeU5UQXpNVE14TkRBM05ESmFNQzR4TERBcUJnTlZCQU1NCkkydDFZbVYyY0c0dGRISmhabVpwWXkxdFlXNWhaMlZ5TG10MVltVjJjRzR1YzNaak1JSUJJakFOQmdrcWhraUcKOXcwQkFRRUZBQU9DQVE4QU1JSUJDZ0tDQVFFQXZkT0J6RXM1bUM4Y21NQ0RyNVFrYlpHWHBkY0Z1MldxR043MQphaEI0dU5ScVNYYTlhMkhTVVp6b0NYWU1LcjFyZjdBclc2dFdJS0w0aUV2bzNMZzNROUduTEFpcThibmp4UjZuClZCSU1CdSs2WHRVVFl0anQ0clE0d0FmNHhqQnBPa2VZQndRd0dQNEFlZVNMdWx1bGNQRVRHeFlQRE5uRmVzak8KZldXOEh3ZFJGNCtvVWM1QkxuRFFVZ2VOSHVTUWpRZTN5OUpiZUFsRTd4dmt4MnVWdzUwSWpsbTNVQmR5Ynd0TApJVHhwSmZrWHdYV3EvN3YxWmxKa09lakU0ZkZsTW1xbzFVWUdrZnN3MVlEOHhXdnE5UmNwV0oyUVZTTm1aVWQxCitsRWJ4TmhicGp5ajByVTFPSm9PeS80eFJHWTR0R0huZllnWlE5VUhRWDlsRWxtZzFRSURBUUFCbzRHM01JRzAKTUIwR0ExVWREZ1FXQkJUNnNqaVJpaG45UDFaNHhCYThOS2NISmVWMDZqQWZCZ05WSFNNRUdEQVdnQlQ2c2ppUgppaG45UDFaNHhCYThOS2NISmVWMDZqQVBCZ05WSFJNQkFmOEVCVEFEQVFIL01HRUdBMVVkRVFSYU1GaUNNV3QxClltVjJjRzR0ZEhKaFptWnBZeTF0WVc1aFoyVnlMbXQxWW1WMmNHNHVjM1pqTG1Oc2RYTjBaWEl1Ykc5allXeUMKSTJ0MVltVjJjRzR0ZEhKaFptWnBZeTF0WVc1aFoyVnlMbXQxWW1WMmNHNHVjM1pqTUEwR0NTcUdTSWIzRFFFQgpDd1VBQTRJQkFRQmMvYjV5SEVHdjRzT3NKYnR4T1NyamxYcFZSaGxIY2VDb05oakJRU2NuUytSTThoS29QRDlGCisvSUZqbXB2VnBYc1FPWXJZeDVXbjJXbS9ZWVdtcFo5WlVrb0tJa1h0VEZmREtpZytkSEI3ek1WVENwZ1p3UzQKdlRyV2U1bE5UWkk5aXlFeEJheThpc0UvUDBNVDRydm0zRlN2cVRlcEdGdEd3TEc1Ymo0Z0gvZzFoTGhrUmpmRgpHVnYzMXBmeW1ZSVdRZW5kcklpLzZaMGVSajVhSUZLM0NhMVl0Ukp3ZnpKNG91SG1SakpGR1hxcVpFeUpFeERzCkw1WTUySnA5MlQ0UE9hQ1VRU0tzWHQ2L09HSTcwNjUrNzZMYjllUVFmQ3RhbHZxVldWeHBQdGFLK1VsTWpvMnIKZlA4eG1DMGdvNDB0R1pUWVh0d2RYNEZmS3NhUGZQRkMKLS0tLS1FTkQgQ0VSVElGSUNBVEUtLS0tLQo="
	str := Base64DecodeToString([]byte(s))
	t.Log(str)
}
