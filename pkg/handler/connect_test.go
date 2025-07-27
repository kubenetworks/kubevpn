package handler

import (
	"net"
	"net/url"
	"reflect"
	"sort"
	"testing"
	"time"

	"github.com/google/gopacket/routing"
	"github.com/libp2p/go-netroute"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/wencaiwulue/kubevpn/v2/pkg/core"
)

func TestRoute(t *testing.T) {
	var r routing.Router
	var err error
	r, err = netroute.New()
	if err != nil {
		t.Fatal(err)
	}
	iface, gateway, src, err := r.Route(net.ParseIP("8.8.8.8"))
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("iface: %s, gateway: %s, src: %s", iface.Name, gateway, src)
}

func TestSort(t *testing.T) {
	list := v1.PodList{
		Items: []v1.Pod{
			{
				ObjectMeta: metav1.ObjectMeta{
					Name:              "a",
					DeletionTimestamp: nil,
				},
			},
			{
				ObjectMeta: metav1.ObjectMeta{
					Name: "b",
					DeletionTimestamp: &metav1.Time{
						Time: time.Now(),
					},
				},
			},
			{
				ObjectMeta: metav1.ObjectMeta{
					Name:              "c",
					DeletionTimestamp: nil,
				},
			},
			{
				ObjectMeta: metav1.ObjectMeta{
					Name:              "d",
					DeletionTimestamp: nil,
				},
			},
		},
	}
	sort.SliceStable(list.Items, func(i, j int) bool {
		return list.Items[i].DeletionTimestamp != nil
	})
	var names []string
	for _, item := range list.Items {
		names = append(names, item.Name)
	}
	equal := reflect.DeepEqual(names, []string{"b", "a", "c", "d"})
	if !equal {
		t.Fatal()
	}
}

func TestParseNode(t *testing.T) {
	type args struct {
		s string
	}
	tests := []struct {
		name    string
		args    args
		want    *core.Node
		wantErr bool
	}{
		{
			name: "",
			args: args{s: "tun:/tcp://remote-addr:9080?net=10.10.10.10/24&gw=10.10.10.1&mtu=1500&route=10.10.10.10/24&route=10.10.10.11/24"},
			want: &core.Node{
				Addr:     "",
				Protocol: "tun",
				Remote:   "tcp://remote-addr:9080",
				Values: url.Values{
					"net": []string{"10.10.10.10/24"},
					"gw":  []string{"10.10.10.1"},
					"mtu": []string{"1500"},
					"route": []string{
						"10.10.10.10/24",
						"10.10.10.11/24",
					},
				},
				Client: nil,
			},
			wantErr: false,
		},
		{
			name: "",
			args: args{s: "tun:/tcp://remote-addr:9080?net=10.10.10.10/24&gw=10.10.10.1&mtu=1500&route=10.10.10.10/24,10.10.10.11/24"},
			want: &core.Node{
				Addr:     "",
				Protocol: "tun",
				Remote:   "tcp://remote-addr:9080",
				Values: url.Values{
					"net": []string{"10.10.10.10/24"},
					"gw":  []string{"10.10.10.1"},
					"mtu": []string{"1500"},
					"route": []string{
						"10.10.10.10/24,10.10.10.11/24",
					},
				},
				Client: nil,
			},
			wantErr: false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := core.ParseNode(tt.args.s)
			if (err != nil) != tt.wantErr {
				t.Errorf("ParseNode() error = %v, wantErr %v", err, tt.wantErr)
				return
			}
			if !reflect.DeepEqual(*got, *tt.want) {
				t.Errorf("ParseNode() got = %v, want %v", got, tt.want)
			}
		})
	}
}
