package tlsconfig

import (
	"crypto/tls"
	"embed"
	log "github.com/sirupsen/logrus"
)

//go:embed server.crt
var crt embed.FS

//go:embed server.key
var key embed.FS

var TlsconfigServer *tls.Config
var TlsconfigClient *tls.Config

func init() {
	crtBytes, _ := crt.ReadFile("server.crt")
	keyBytes, _ := key.ReadFile("server.key")
	pair, err := tls.X509KeyPair(crtBytes, keyBytes)
	if err != nil {
		log.Fatal(err)
	}
	TlsconfigServer = &tls.Config{
		Certificates: []tls.Certificate{pair},
	}

	TlsconfigClient = &tls.Config{
		Certificates:       []tls.Certificate{pair},
		InsecureSkipVerify: true,
	}
}
