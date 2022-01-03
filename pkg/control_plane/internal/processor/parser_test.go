/*
* Copyright (C) 2021 THL A29 Limited, a Tencent company.  All rights reserved.
* This source code is licensed under the Apache License Version 2.0.
 */

package processor

import (
	"fmt"
	"github.com/wencaiwulue/kubevpn/pkg/control_plane/apis/v1alpha1"
	"github.com/wencaiwulue/kubevpn/util"
	"gopkg.in/yaml.v2"
	"testing"
)

func TestName(t *testing.T) {
	var config = v1alpha1.EnvoyConfig{
		Name: "config-test",
		Spec: v1alpha1.Spec{
			Listeners: []v1alpha1.Listener{
				{
					Name:    "listener1",
					Address: "127.0.0.1",
					Port:    15006,
					Routes: []v1alpha1.Route{
						{
							Name: "route-0",
							Headers: []v1alpha1.HeaderMatch{
								{
									Key:   "a",
									Value: "aa",
								},
								{
									Key:   "b",
									Value: "bb",
								},
							},
							ClusterNames: []string{"cluster0"},
						}, {
							Name: "route-1",
							Headers: []v1alpha1.HeaderMatch{
								{
									Key:   "c",
									Value: "cc",
								},
								{
									Key:   "d",
									Value: "dd",
								},
							},
							ClusterNames: []string{"cluster-1"},
						},
					},
				},
			},
			Clusters: []v1alpha1.Cluster{
				{
					Name: "cluster-0",
					Endpoints: []v1alpha1.Endpoint{
						{
							Address: "127.0.0.1",
							Port:    9101,
						},
						{
							Address: "127.0.0.1",
							Port:    9102,
						},
					},
				}, {
					Name: "cluster-1",
					Endpoints: []v1alpha1.Endpoint{
						{
							Address: "127.0.0.1",
							Port:    9103,
						},
						{
							Address: "127.0.0.1",
							Port:    9104,
						},
					},
				},
			},
		},
	}
	marshal, _ := yaml.Marshal(config)
	fmt.Println(string(marshal))
}

func TestName1(t *testing.T) {
	parseYaml, err := util.ParseYamlBytes([]byte(sss))
	fmt.Println(err)
	fmt.Println(parseYaml)
}

var sss = `name: config-test
spec:
  listeners:
  - name: listener1
    address: 127.0.0.1
    port: 15006
    routes:
    - name: route-0
      clusters:
      - cluster-0
  clusters:
  - name: cluster-0
    endpoints:
    - address: 127.0.0.1
      port: 9080
`
