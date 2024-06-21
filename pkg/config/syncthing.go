package config

import (
	"crypto/tls"

	"github.com/syncthing/syncthing/lib/protocol"
)

const (
	SyncthingDir = "syncthing"

	SyncthingGUIDir = "gui"

	DefaultRemoteDir = "/kubevpn-data"
)

var LocalCert tls.Certificate
var RemoteCert tls.Certificate
var LocalDeviceID protocol.DeviceID
var RemoteDeviceID protocol.DeviceID

const (
	SyncthingLocalDeviceID = "BSNCBRY-ZI5HLYC-YH6544V-SQ3IDKT-4JQKING-ZGSW463-UKYEYCA-WO7ZHA3"
	SyncthingLocalCert     = `-----BEGIN CERTIFICATE-----
MIICHjCCAaSgAwIBAgIIHY0CWDFbXYEwCgYIKoZIzj0EAwIwSjESMBAGA1UEChMJ
U3luY3RoaW5nMSAwHgYDVQQLExdBdXRvbWF0aWNhbGx5IEdlbmVyYXRlZDESMBAG
A1UEAxMJc3luY3RoaW5nMCAXDTI0MDYxOTAwMDAwMFoYDzE4NTQwOTExMDA1MDUy
WjBKMRIwEAYDVQQKEwlTeW5jdGhpbmcxIDAeBgNVBAsTF0F1dG9tYXRpY2FsbHkg
R2VuZXJhdGVkMRIwEAYDVQQDEwlzeW5jdGhpbmcwdjAQBgcqhkjOPQIBBgUrgQQA
IgNiAAQj1ov1aM0902yssK+3LPiGM1e1pUcVRuQjxl0nDX0fpZp3kdeWeiBm9AlE
uwhAll/8QjoWBlNiEXyGFN9lOaIGf7ZIk7owPT6LiJXc1n3E6iqHWeSXcZ9dJL7M
+E4eleajVTBTMA4GA1UdDwEB/wQEAwIFoDAdBgNVHSUEFjAUBggrBgEFBQcDAQYI
KwYBBQUHAwIwDAYDVR0TAQH/BAIwADAUBgNVHREEDTALgglzeW5jdGhpbmcwCgYI
KoZIzj0EAwIDaAAwZQIwJI4KA9JgFXWU4dWq6JnIr+lAuIJ5ON2lFPrX8JWi1Z3F
UXrvm80w+uR+1rLt6AdkAjEA3dpoBnS7tV21krEVmfX2vabtkzZidhXwuvP+1VJN
By4EwZnuTLX3TqQx2TERF9rV
-----END CERTIFICATE-----
`
	SyncthingLocalKey = `-----BEGIN EC PRIVATE KEY-----
MIGkAgEBBDAltfhZ8YO4CrPsvFRpU6P8lOspm5VXFGvJghSaDr4D/ub66+4HpTk9
3TdgtbUSMSmgBwYFK4EEACKhZANiAAQj1ov1aM0902yssK+3LPiGM1e1pUcVRuQj
xl0nDX0fpZp3kdeWeiBm9AlEuwhAll/8QjoWBlNiEXyGFN9lOaIGf7ZIk7owPT6L
iJXc1n3E6iqHWeSXcZ9dJL7M+E4eleY=
-----END EC PRIVATE KEY-----
`
)

const (
	SyncthingRemoteDeviceID = "OELB2JL-MIOW652-6JPBYPZ-POV3EBV-XEOW2Z2-I45QUGZ-QF5TT4P-Z2AH7AU"
	SyncthingRemoteCert     = `-----BEGIN CERTIFICATE-----
MIICHzCCAaWgAwIBAgIJAOGCLdtwnUShMAoGCCqGSM49BAMCMEoxEjAQBgNVBAoT
CVN5bmN0aGluZzEgMB4GA1UECxMXQXV0b21hdGljYWxseSBHZW5lcmF0ZWQxEjAQ
BgNVBAMTCXN5bmN0aGluZzAgFw0yNDA2MTkwMDAwMDBaGA8xODU0MDkxMTAwNTA1
MlowSjESMBAGA1UEChMJU3luY3RoaW5nMSAwHgYDVQQLExdBdXRvbWF0aWNhbGx5
IEdlbmVyYXRlZDESMBAGA1UEAxMJc3luY3RoaW5nMHYwEAYHKoZIzj0CAQYFK4EE
ACIDYgAETwaM3V92D499uMXWFgGxdTUAvtp1tN7ePuJxt8W+FO0izG1fa7oU29Hp
FU0Ohh3xwnQfEHIWzlKJllZ2ZbbXGOvcfr0Yfiir6ToKuN6185EA8RHkA+5HRtu5
nw5wyWL/o1UwUzAOBgNVHQ8BAf8EBAMCBaAwHQYDVR0lBBYwFAYIKwYBBQUHAwEG
CCsGAQUFBwMCMAwGA1UdEwEB/wQCMAAwFAYDVR0RBA0wC4IJc3luY3RoaW5nMAoG
CCqGSM49BAMCA2gAMGUCMGxR9q9vjzm4GynOkoRIC+BQJN0zpiNusYUD6iYJNGe1
wNH8jhOJEG+rjGracDZ6bgIxAIpyHv/rOAjEX7/wcafRqGTFhwXdRq0l3493aERd
RCwqD8rbzP0QStVOCAE7xYt/sQ==
-----END CERTIFICATE-----
`
	SyncthingRemoteKey = `-----BEGIN EC PRIVATE KEY-----
MIGkAgEBBDAKabOokHf64xAsIQp5PA1zZ5vLjfcgKcuikx/D0CP6c2Cf48a6eADE
GWrY1Ng8UzOgBwYFK4EEACKhZANiAARPBozdX3YPj324xdYWAbF1NQC+2nW03t4+
4nG3xb4U7SLMbV9ruhTb0ekVTQ6GHfHCdB8QchbOUomWVnZlttcY69x+vRh+KKvp
Ogq43rXzkQDxEeQD7kdG27mfDnDJYv8=
-----END EC PRIVATE KEY-----
`
)

func init() {
	var err error
	LocalCert, err = tls.X509KeyPair([]byte(SyncthingLocalCert), []byte(SyncthingLocalKey))
	if err != nil {
		panic(err)
	}
	RemoteCert, err = tls.X509KeyPair([]byte(SyncthingRemoteCert), []byte(SyncthingRemoteKey))
	if err != nil {
		panic(err)
	}
	LocalDeviceID, err = protocol.DeviceIDFromString(SyncthingLocalDeviceID)
	if err != nil {
		panic(err)
	}
	RemoteDeviceID, err = protocol.DeviceIDFromString(SyncthingRemoteDeviceID)
	if err != nil {
		panic(err)
	}
}
