package config

import (
	"crypto/tls"
	"embed"

	log "github.com/sirupsen/logrus"
)

//go:embed server.crt
var crt embed.FS

//go:embed server.key
var key embed.FS

var TlsConfigServer *tls.Config
var TlsConfigClient *tls.Config

func init() {
	crtBytes, _ := crt.ReadFile("server.crt")
	keyBytes, _ := key.ReadFile("server.key")
	pair, err := tls.X509KeyPair(crtBytes, keyBytes)
	if err != nil {
		log.Fatal(err)
	}
	TlsConfigServer = &tls.Config{
		Certificates: []tls.Certificate{pair},
	}

	TlsConfigClient = &tls.Config{
		Certificates:       []tls.Certificate{pair},
		InsecureSkipVerify: true,
	}
}
