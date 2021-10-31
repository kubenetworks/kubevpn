package tlsconfig

import (
	"crypto/tls"
	"fmt"
	log "github.com/sirupsen/logrus"
	"github.com/wencaiwulue/kubevpn/util"
	"io"
	"net"
	"testing"
)

func init() {
	util.InitLogger(true)
}

func TestName(t *testing.T) {
	listen, _ := net.Listen("tcp", ":9090")
	listener := tls.NewListener(listen, TlsconfigServer)
	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				log.Errorln(err)
			}
			go func(conn net.Conn) {
				bytes := make([]byte, 1024)
				all, err2 := conn.Read(bytes)
				if err2 != nil {
					log.Errorln(err2)
					return
				}
				defer conn.Close()
				fmt.Println(string(bytes[:all]))
				io.WriteString(conn, "hello client")
			}(conn)
		}
	}()
	dial, err := net.Dial("tcp", ":9090")
	if err != nil {
		log.Errorln(err)
	}

	client := tls.Client(dial, TlsconfigClient)
	client.Write([]byte("hi server"))
	all, err := io.ReadAll(client)
	if err != nil {
		log.Errorln(err)
	}
	fmt.Println(string(all))
}
