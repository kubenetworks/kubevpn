package cmds

import (
	"context"
	"reflect"
	"testing"

	plog "github.com/wencaiwulue/kubevpn/v2/pkg/log"
)

func TestAlias(t *testing.T) {
	str := `Name: test
Needs: test1
Flags:
  - --kubeconfig=~/.kube/config
  - --namespace=test

---

Name: test1
Flags:
  - --kubeconfig=~/.kube/config
  - --namespace=test
  - --extra-hosts=xxx.com`
	_, err := ParseConfig([]byte(str))
	if err != nil {
		plog.G(context.Background()).Fatal(err)
	}
}

func TestCheckLoop(t *testing.T) {
	str := `Name: test
Needs: test1
Flags:
  - --kubeconfig=~/.kube/config
  - --namespace=test

---

Name: test1
Flags:
  - --kubeconfig=~/.kube/config
  - --namespace=test
  - --extra-hosts=xxx.com`
	_, err := ParseConfig([]byte(str))
	if err != nil {
		plog.G(context.Background()).Fatal(err)
	}
}

func TestLoop(t *testing.T) {
	data := []struct {
		Config      string
		Run         string
		ExpectError bool
		ExpectOrder []string
	}{
		{
			Config: `
Name: test
Needs: test1
Flags:
  - --kubeconfig=~/.kube/config
  - --namespace=test

---

Name: test1
Needs: test
Flags:
  - --kubeconfig=~/.kube/config
  - --namespace=test
  - --extra-hosts=xxx.com
`,
			Run:         "test",
			ExpectError: true,
			ExpectOrder: nil,
		},
		{
			Config: `
Name: test
Needs: test1
Flags:
  - --kubeconfig=~/.kube/config
  - --namespace=test

---

Name: test1
Flags:
  - --kubeconfig=~/.kube/config
  - --namespace=test
  - --extra-hosts=xxx.com
`,
			Run:         "test",
			ExpectError: false,
			ExpectOrder: []string{"test1", "test"},
		},
		{
			Config: `
Name: test
Needs: test1
Flags:
  - --kubeconfig=~/.kube/config
  - --namespace=test

---

Name: test1
Needs: test2
Flags:
  - --kubeconfig=~/.kube/config
  - --namespace=test
  - --extra-hosts=xxx.com
`,
			Run:         "test",
			ExpectError: false,
			ExpectOrder: []string{"test1", "test"},
		},
		{
			Config: `
Name: test
Needs: test1
Flags:
  - --kubeconfig=~/.kube/config
  - --namespace=test

---

Name: test1
Needs: test2
Flags:
  - --kubeconfig=~/.kube/config
  - --namespace=test
  - --extra-hosts=xxx.com

---

Name: test2
Flags:
  - --kubeconfig=~/.kube/config
  - --namespace=test
  - --extra-hosts=xxx.com
`,
			Run:         "test",
			ExpectError: false,
			ExpectOrder: []string{"test2", "test1", "test"},
		},
		{
			Config: `
Name: test
Needs: test1
Flags:
  - --kubeconfig=~/.kube/config
  - --namespace=test

---

Name: test1
Needs: test2
Flags:
  - --kubeconfig=~/.kube/config
  - --namespace=test
  - --extra-hosts=xxx.com

---

Name: test2
Flags:
  - --kubeconfig=~/.kube/config
  - --namespace=test
  - --extra-hosts=xxx.com
`,
			Run:         "test2",
			ExpectError: false,
			ExpectOrder: []string{"test2"},
		},
		{
			Config: `
Name: test
Needs: test1
Flags:
  - --kubeconfig=~/.kube/config
  - --namespace=test

---

Name: test1
Needs: test2
Flags:
  - --kubeconfig=~/.kube/config
  - --namespace=test
  - --extra-hosts=xxx.com

---

Name: test2
Flags:
  - --kubeconfig=~/.kube/config
  - --namespace=test
  - --extra-hosts=xxx.com
`,
			Run:         "test1",
			ExpectError: false,
			ExpectOrder: []string{"test2", "test1"},
		},
	}
	for _, datum := range data {
		configs, err := ParseConfig([]byte(datum.Config))
		if err != nil {
			plog.G(context.Background()).Fatal(err)
		}
		getConfigs, err := GetConfigs(configs, datum.Run)
		if err != nil && !datum.ExpectError {
			plog.G(context.Background()).Fatal(err)
		} else if err != nil {
		}
		if datum.ExpectError {
			continue
		}
		var c []string
		for _, config := range getConfigs {
			c = append(c, config.Name)
		}
		if !reflect.DeepEqual(c, datum.ExpectOrder) {
			plog.G(context.Background()).Fatalf("Not match, expect: %v, real: %v", datum.ExpectOrder, c)
		}
	}
}
