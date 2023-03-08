package util

import (
	"encoding/json"
	"testing"

	"github.com/containernetworking/cni/libcni"
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
		log.Infoln("get cni config", configList.Name)
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
