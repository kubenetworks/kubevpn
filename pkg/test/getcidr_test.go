package main

import (
	"context"
	"fmt"
	"testing"

	pkgutil "github.com/wencaiwulue/kubevpn/v2/pkg/util"
)

func TestByDumpClusterInfo(t *testing.T) {
	Init()
	info, err := pkgutil.GetCIDRByDumpClusterInfo(context.Background(), clientset)
	if err != nil {
		t.Error(err)
	}
	for _, ipNet := range info {
		fmt.Println(ipNet.String())
	}
}

func TestByCreateService(t *testing.T) {
	Init()
	info, err := pkgutil.GetServiceCIDRByCreateService(context.Background(), clientset.CoreV1().Services("default"))
	if err != nil {
		t.Error(err)
	}
	fmt.Println(info)
}

func TestElegant(t *testing.T) {
	Init()
	elegant, err := pkgutil.GetCIDRElegant(context.Background(), clientset, restclient, restconfig, namespace)
	if err != nil {
		t.Error(err)
	}
	for _, net := range elegant {
		fmt.Println(net.String())
	}
}
