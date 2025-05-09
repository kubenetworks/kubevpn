package handler

import (
	"net"
	"reflect"
	"sort"
	"testing"
	"time"

	"github.com/google/gopacket/routing"
	"github.com/libp2p/go-netroute"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
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
