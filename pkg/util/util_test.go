package util

import (
	"encoding/json"
	"net"
	"strings"
	"testing"

	"github.com/containernetworking/cni/libcni"
	"github.com/google/gopacket"
	"github.com/google/gopacket/examples/util"
	"github.com/google/gopacket/layers"
	log "github.com/sirupsen/logrus"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

func TestName(t *testing.T) {
	var s = `
{
  "name": "cni0",
  "cniVersion":"0.3.1",
  "plugins":[
    {
      "datastore_type": "kubernetes",
      "nodename": "172.19.37.35",
      "type": "calico",
      "log_level": "info",
      "log_file_path": "/var/log/calico/cni/cni.log",
      "ipam": {
        "type": "calico-ipam",
        "assign_ipv4": "true",
        "ipv4_pools": ["10.233.64.0/18", "10.233.64.0/19", "fe80:0000:0000:0000:0204:61ff:fe9d:f156/100"]
      },
      "policy": {
        "type": "k8s"
      },
      "kubernetes": {
        "kubeconfig": "/etc/cni/net.d/calico-kubeconfig"
      }
    },
    {
      "type":"portmap",
      "capabilities": {
        "portMappings": true
      }
    }
  ]
}
`

	// IPv6 with CIDR
	configList, err := libcni.ConfListFromBytes([]byte(s))
	if err == nil {
		log.Infoln("Get CNI config", configList.Name)
	}
	for _, plugin := range configList.Plugins {
		var m map[string]interface{}
		_ = json.Unmarshal(plugin.Bytes, &m)
		slice, _, _ := unstructured.NestedStringSlice(m, "ipam", "ipv4_pools")
		for _, i := range slice {
			println(i)
		}
	}
}

func TestPing(t *testing.T) {
	defer util.Run()()
	SrcIP := net.ParseIP("223.254.0.102").To4()
	DstIP := net.ParseIP("223.254.0.100").To4()

	icmpLayer := layers.ICMPv4{
		TypeCode: layers.CreateICMPv4TypeCode(layers.ICMPv4TypeEchoRequest, 0),
		Id:       8888,
		Seq:      1,
	}
	ipLayer := layers.IPv4{
		Version:  4,
		SrcIP:    SrcIP,
		DstIP:    DstIP,
		Protocol: layers.IPProtocolICMPv4,
		Flags:    layers.IPv4DontFragment,
		TTL:      64,
		IHL:      5,
		Id:       55664,
	}
	opts := gopacket.SerializeOptions{
		FixLengths:       true,
		ComputeChecksums: true,
	}
	buf := gopacket.NewSerializeBuffer()
	err := gopacket.SerializeLayers(buf, opts, &icmpLayer, &ipLayer)
	if err != nil {
		log.Errorf("Failed to serialize icmp packet, err: %v", err)
		return
	}
	ipConn, err := net.ListenPacket("ip4:icmp", "localhost")
	if err != nil {
		if strings.Contains(err.Error(), "operation not permitted") {
			return
		}
		t.Error(err)
	}
	bytes := buf.Bytes()

	_, err = ipConn.WriteTo(bytes, &net.IPAddr{IP: ipLayer.DstIP})
	if err != nil {
		t.Error(err)
	}
	log.Print("Packet sent!")
}
