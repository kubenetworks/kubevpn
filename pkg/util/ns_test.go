package util

import (
	"os"
	"testing"

	"k8s.io/cli-runtime/pkg/genericclioptions"
	"k8s.io/client-go/rest"
	clientcmdapi "k8s.io/client-go/tools/clientcmd/api"
	cmdutil "k8s.io/kubectl/pkg/cmd/util"
	"k8s.io/utils/ptr"
)

func TestMergeRawConfig(t *testing.T) {
	var kubeConfigBytes = `
apiVersion: v1
kind: Config
clusters:
  - cluster:
      server: http://localhost:8001
    name: localhost
  - cluster:
      server: http://localhost:8002
    name: localhost2
contexts:
  - name: localhost
    context:
      cluster: localhost
      namespace: test
  - name: localhost2
    context:
      cluster: localhost2
      namespace: test2
current-context: localhost`

	temp, err := os.CreateTemp("", "")
	if err != nil {
		t.Fatal(err)
	}
	temp.Close()
	t.Cleanup(func() {
		_ = os.Remove(temp.Name())
	})
	err = os.WriteFile(temp.Name(), []byte(kubeConfigBytes), 0644)
	if err != nil {
		t.Fatal(err)
	}
	testData := []struct {
		Context   string
		Namespace string
		ApiServer string
	}{
		{
			Context:   "localhost2",
			ApiServer: "http://localhost:8002",
			Namespace: "test2",
		},
		{
			Context:   "localhost",
			ApiServer: "http://localhost:8001",
			Namespace: "test",
		},
		{
			Context:   "",
			ApiServer: "http://localhost:8001",
			Namespace: "test",
		},
	}

	for _, data := range testData {
		configFlags := genericclioptions.NewConfigFlags(true)
		configFlags.KubeConfig = ptr.To(temp.Name())
		configFlags.Context = ptr.To(data.Context)
		matchVersionFlags := cmdutil.NewMatchVersionFlags(configFlags)
		factory := cmdutil.NewFactory(matchVersionFlags)
		var bytes []byte
		var ns string
		bytes, ns, err = ConvertToKubeConfigBytes(factory)
		if err != nil {
			t.Fatal(err)
		}
		if ns != data.Namespace {
			t.Fatalf("not equal")
		}
		newFactory := InitFactory(string(bytes), ns)
		var config *rest.Config
		config, err = newFactory.ToRESTConfig()
		if err != nil {
			t.Fatal(err)
		}
		var rawConfig clientcmdapi.Config
		rawConfig, err = newFactory.ToRawKubeConfigLoader().RawConfig()
		if err != nil {
			t.Fatal(err)
		}
		if config.Host != data.ApiServer {
			t.Fatalf("apiserver not match. current: %s, expect: %s", config.Host, data.ApiServer)
		}
		if data.Context == "" {
			continue
		}
		if rawConfig.CurrentContext != data.Context {
			t.Fatalf("context not match. current: %s, expect: %s", rawConfig.CurrentContext, data.Context)
		}
	}
}
