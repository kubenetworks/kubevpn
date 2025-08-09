package cmds

import (
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"

	"github.com/spf13/cobra"
	"k8s.io/apimachinery/pkg/util/sets"
	cmdutil "k8s.io/kubectl/pkg/cmd/util"
	"k8s.io/kubectl/pkg/util/i18n"
	"k8s.io/kubectl/pkg/util/templates"
	yaml "sigs.k8s.io/yaml/goyaml.v3"

	"github.com/wencaiwulue/kubevpn/v2/pkg/config"
	"github.com/wencaiwulue/kubevpn/v2/pkg/util"
)

// CmdAlias
/**
Name: test
Description: this is a test environment
Needs: test1
Flags:
  - connect
  - --kubeconfig=~/.kube/config
  - --namespace=test
---

Name: test1
Description: this is another test environment
Flags:
  - connect
  - --kubeconfig=~/.kube/jumper_config
  - --namespace=test
  - --extra-hosts=xxx.com
*/
func CmdAlias(f cmdutil.Factory) *cobra.Command {
	var localFile, remoteAddr string
	cmd := &cobra.Command{
		Use:   "alias",
		Short: i18n.T("Config file alias to execute command simply"),
		Long: templates.LongDesc(i18n.T(`
		Config file alias to execute command simply, just like ssh alias config

		Please point to an existing, complete config file:

		1. Via the command-line flag --kubevpnconfig
		2. Via the KUBEVPNCONFIG environment variable
		3. In your home directory as ~/.kubevpn/config.yaml

		It will read ~/.kubevpn/config.yaml file as config, also support special file path
		by flag -f. It also supports depends relationship, like one cluster api server needs to 
		access via another cluster, you can use syntax needs. it will do action to needs cluster first
 		and then do action to target cluster
		`)),
		Example: templates.Examples(i18n.T(`
		If you have following config in your ~/.kubevpn/config.yaml

		Name: dev
		Description: This is dev k8s environment
		Needs: jumper
		Flags:
		  - connect
		  - --kubeconfig=~/.kube/config
		  - --namespace=default
		---
		
		Name: jumper
		Description: This is jumper k8s environment
		Flags:
		  - connect
		  - --kubeconfig=~/.kube/jumper_config
		  - --namespace=test
		  - --extra-hosts=xxx.com

		Name: all-in-one
		Description: use special flags '--kubeconfig-json', no need to special kubeconfig path
		Flags:
		  - connect
		  - --kubeconfig-json={"apiVersion":"v1","clusters":[{"cluster":{"certificate-authority-data":"LS0tLS1CRU..."}}]}
		  - --namespace=test
		  - --extra-hosts=xxx.com
		
		Config file support three field: Name,Needs,Flags

		# Use kubevpn alias config to simply execute command, connect to cluster network by order: jumper --> dev
		kubevpn alias dev
		
		# kubevpn alias jumper, just connect to cluster jumper
		kubevpn alias jumper

		# support special flags '--kubeconfig-json', it will save kubeconfig into ~/.kubevpn/temp/[ALIAS_NAME]
        kubevpn alias all-in-one
		`)),
		PreRunE: func(cmd *cobra.Command, args []string) (err error) {
			if localFile != "" {
				_, err = os.Stat(localFile)
			}
			return err
		},
		Args: cobra.MatchAll(cobra.ExactArgs(1)),
		RunE: func(cmd *cobra.Command, args []string) error {
			configs, err := ParseAndGet(localFile, remoteAddr, args[0])
			if err != nil {
				return err
			}
			name, err := os.Executable()
			if err != nil {
				return err
			}
			for _, conf := range configs {
				err = ParseArgs(cmd, &conf)
				if err != nil {
					return err
				}
				c := exec.Command(name, conf.Flags...)
				c.Stdout = os.Stdout
				c.Stdin = os.Stdin
				c.Stderr = os.Stderr
				fmt.Println(fmt.Sprintf("Name: %s", conf.Name))
				if conf.Description != "" {
					fmt.Println(fmt.Sprintf("Description: %s", conf.Description))
				}
				fmt.Println(fmt.Sprintf("Command: %v", c.Args))
				err = c.Run()
				if err != nil {
					return err
				}
			}
			return nil
		},
	}
	cmd.Flags().StringVarP(&localFile, "kubevpnconfig", "f", util.If(os.Getenv("KUBEVPNCONFIG") != "", os.Getenv("KUBEVPNCONFIG"), config.GetConfigFile()), "Path to the kubevpnconfig file to use for CLI requests.")
	cmd.Flags().StringVarP(&remoteAddr, "remote", "r", "", "Remote config file, eg: https://raw.githubusercontent.com/kubenetworks/kubevpn/master/pkg/config/config.yaml")
	return cmd
}

func ParseAndGet(localFile, remoteAddr string, aliasName string) ([]Config, error) {
	var content []byte
	var err error
	var path string
	if localFile != "" {
		path = localFile
		content, err = os.ReadFile(path)
	} else if remoteAddr != "" {
		path = remoteAddr
		content, err = util.DownloadFileStream(path)
	} else {
		path = config.GetConfigFile()
		content, err = os.ReadFile(path)
	}
	if err != nil {
		return nil, err
	}
	list, err := ParseConfig(content)
	if err != nil {
		return nil, err
	}
	configs, err := GetConfigs(list, aliasName)
	if err != nil {
		return nil, err
	}
	if len(configs) == 0 {
		var names []string
		for _, c := range list {
			if c.Name != "" {
				names = append(names, c.Name)
			}
		}
		err = errors.New(fmt.Sprintf("Can't find any alias for the name: '%s', avaliable: \n[\"%s\"]\nPlease check config file: %s", aliasName, strings.Join(names, "\", \""), path))
		return nil, err
	}
	return configs, nil
}

func ParseConfig(file []byte) ([]Config, error) {
	decoder := yaml.NewDecoder(strings.NewReader(string(file)))
	var configs []Config
	for {
		var cfg Config
		err := decoder.Decode(&cfg)
		if err != nil {
			if err == io.EOF {
				break
			}
			return nil, err
		}
		configs = append(configs, cfg)
	}
	return configs, nil
}

func GetConfigs(configs []Config, name string) ([]Config, error) {
	m := make(map[string]Config)
	for _, config := range configs {
		m[config.Name] = config
	}
	var result []Config
	var set []string
	for !sets.New[string](set...).Has(name) {
		config, ok := m[name]
		if ok {
			result = append([]Config{config}, result...)
			set = append(set, name)
			name = config.Needs
			if name == "" {
				return result, nil
			}
		} else {
			return result, nil
		}
	}
	return nil, fmt.Errorf("loop jump detected: %s. verify your configuration", strings.Join(append(set, name), " -> "))
}

type Config struct {
	Name        string   `yaml:"Name"`
	Description string   `yaml:"Description"`
	Needs       string   `yaml:"Needs,omitempty"`
	Flags       []string `yaml:"Flags,omitempty"`
}
