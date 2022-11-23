package dns

import (
	"fmt"
	miekgdns "github.com/miekg/dns"
	"io/fs"
	"io/ioutil"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/wencaiwulue/kubevpn/pkg/util"
)

func TestSetupDnsServer(t *testing.T) {
	port := util.GetAvailableUDPPortOrDie()
	clientConfig := &miekgdns.ClientConfig{
		Servers: []string{"10.233.93.190"},
		Search:  []string{"vke-system.svc.cluster.local", "svc.cluster.local", "cluster.local"},
		Port:    "53",
		Ndots:   0,
	}
	go func() {
		err := NewDNSServer("udp", "127.0.0.1:"+strconv.Itoa(port), clientConfig)
		if err != nil {
			t.FailNow()
		}
	}()
	config := miekgdns.ClientConfig{
		Servers: []string{"127.0.0.1"},
		Search:  clientConfig.Search,
		Port:    strconv.Itoa(port),
		Ndots:   clientConfig.Ndots,
		Timeout: 1,
	}
	_ = os.RemoveAll(filepath.Join("/", "etc", "resolver"))
	if err := os.MkdirAll(filepath.Join("/", "etc", "resolver"), fs.ModePerm); err != nil {
		panic(err)
	}
	for _, s := range strings.Split(clientConfig.Search[0], ".") {
		filename := filepath.Join("/", "etc", "resolver", s)
		err := ioutil.WriteFile(filename, []byte(toString(config)), 0644)
		if err != nil {
			panic(err)
		}
	}
	fmt.Println(port)
	select {}
}

func TestFull(t *testing.T) {
	type Question struct {
		Q string
	}
	type person struct {
		Name     string
		age      *int
		Question []Question
	}

	age := 22
	p := &person{"Bob", &age, []Question{{"haha"}}}
	fmt.Println(p)

	p2 := new(person)
	*p2 = *p
	fmt.Println(p2)
	p.Name = " zhangsan"
	p.Question = append(p.Question, Question{"asdf"})
	fmt.Println(p.Question)

	fmt.Println(p2.Question)
}

func TestName(t *testing.T) {
	type name struct {
		input  string
		output string
	}
	var data = []name{
		{
			input:  "ry-server",
			output: "ry-server.vke-system.svc.cluster.local",
		},
		{
			input:  "ry-server.",
			output: "ry-server.vke-system.svc.cluster.local",
		},
		{
			input:  "ry-server.vke-system",
			output: "ry-server.vke-system.svc.cluster.local",
		}, {
			input:  "ry-server.vke-system.",
			output: "ry-server.vke-system.svc.cluster.local",
		},
		{
			input:  "ry-server.vke-system.svc",
			output: "ry-server.vke-system.svc.cluster.local",
		},
		{
			input:  "ry-server.vke-system.svc.",
			output: "ry-server.vke-system.svc.cluster.local",
		},
		{
			input:  "ry-server.vke-system.svc.cluster",
			output: "ry-server.vke-system.svc.cluster.local",
		},
		{
			input:  "mongodb-1.mongodb-headless",
			output: "mongodb-1.mongodb-headless.vke-system.svc.cluster.local",
		}, {
			input:  "mongodb-1.mongodb-headless.",
			output: "mongodb-1.mongodb-headless.vke-system.svc.cluster.local",
		},
		{
			input:  "mongodb-1.mongodb-headless.vke-system",
			output: "mongodb-1.mongodb-headless.vke-system.svc.cluster.local",
		},
		{
			input:  "mongodb-1.mongodb-headless.vke-system.",
			output: "mongodb-1.mongodb-headless.vke-system.svc.cluster.local",
		},
		{
			input:  "mongodb-1.mongodb-headless.vke-system.svc",
			output: "mongodb-1.mongodb-headless.vke-system.svc.cluster.local",
		},
		{
			input:  "mongodb-1.mongodb-headless.vke-system.svc.cluster",
			output: "mongodb-1.mongodb-headless.vke-system.svc.cluster.local",
		},
	}
	for _, datum := range data {
		if o := fix(datum.input, "vke-system.svc.cluster.local"); o != datum.output {
			t.Logf("output: %s, expected: %s", o, datum.output)
			t.FailNow()
		}
	}
}
