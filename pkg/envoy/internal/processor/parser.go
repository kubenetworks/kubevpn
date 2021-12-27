package processor

import (
	"fmt"
	"github.com/wencaiwulue/kubevpn/pkg/envoy/apis/v1alpha1"
	"gopkg.in/yaml.v2"
	"io/ioutil"
)

// parseYaml takes in a yaml envoy config and returns a typed version
func parseYaml(file string) (*v1alpha1.EnvoyConfig, error) {
	var config v1alpha1.EnvoyConfig

	yamlFile, err := ioutil.ReadFile(file)
	if err != nil {
		return nil, fmt.Errorf("Error reading YAML file: %s\n", err)
	}

	err = yaml.Unmarshal(yamlFile, &config)
	if err != nil {
		return nil, err
	}

	return &config, nil
}
