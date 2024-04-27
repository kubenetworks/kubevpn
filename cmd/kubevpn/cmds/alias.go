package cmds

import (
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

	"github.com/wencaiwulue/kubevpn/v2/pkg/daemon"
	"github.com/wencaiwulue/kubevpn/v2/pkg/util"
)

// CmdAlias
/**
Name: test
Needs: test1
Flags:
  - connect
  - --kubeconfig ~/.kube/config
  - --namespace test
  - --lite
---

Name: test1
Flags:
  - connect
  - --kubeconfig ~/.kube/jumper_config
  - --namespace test
  - --extra-hosts xxx.com
*/
func CmdAlias(f cmdutil.Factory) *cobra.Command {
	var localFile, remoteAddr string
	cmd := &cobra.Command{
		Use:   "alias",
		Short: "Config file alias to execute command simply",
		Long:  templates.LongDesc(i18n.T(`Config file alias to execute command simply, just like ssh alias config`)),
		Example: templates.Examples(i18n.T(`
		If you have following config in your ~/.kubevpn/config.yaml

Name: dev
Needs: jumper
Flags:
  - connect
  - --kubeconfig ~/.kube/config
  - --namespace default
  - --lite
---

Name: jumper
Flags:
  - connect
  - --kubeconfig ~/.kube/jumper_config
  - --namespace test
  - --extra-hosts xxx.com

Config file support three field: Name,Needs,Flags

		# Use kubevpn alias config to simply execute command, connect to cluster network by order: jumper --> dev
		kubevpn alias dev
		
		# kubevpn alias jumper, just connect to cluster jumper
		kubevpn alias jumper
`)),
		PreRunE: func(cmd *cobra.Command, args []string) (err error) {
			if localFile != "" {
				_, err = os.Stat(localFile)
			}
			return err
		},
		Args: cobra.MatchAll(cobra.ExactArgs(1)),
		RunE: func(cmd *cobra.Command, args []string) error {
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
				path = daemon.GetConfigFilePath()
				content, err = os.ReadFile(path)
			}
			if err != nil {
				return err
			}
			configList, err := ParseConfig(content)
			if err != nil {
				return err
			}
			configs, err := GetConfigs(configList, args[0])
			if len(configs) == 0 {
				return fmt.Errorf("can not found any alias for name %s, please check your config file %s", args[0], path)
			}
			name, err := os.Executable()
			if err != nil {
				return err
			}
			for _, config := range configs {
				c := exec.Command(name, strings.Split(strings.Join(config.Flags, " "), " ")...)
				c.Stdout = os.Stdout
				c.Stdin = os.Stdin
				c.Stderr = os.Stderr
				fmt.Println(c.Args)
				err = c.Run()
				if err != nil {
					return err
				}
			}
			return nil
		},
	}
	cmd.Flags().StringVarP(&localFile, "file", "f", daemon.GetConfigFilePath(), "Config file location")
	cmd.Flags().StringVarP(&remoteAddr, "remote", "r", "", "Remote config file, eg: https://raw.githubusercontent.com/kubenetworks/kubevpn/master/pkg/config/config.yaml")
	return cmd
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
	return nil, fmt.Errorf("detect loop jump: %s, please check your config", strings.Join(append(set, name), " -> "))
}

type Config struct {
	Name  string   `yaml:"Name"`
	Needs string   `yaml:"Needs,omitempty"`
	Flags []string `yaml:"Flags,omitempty"`
}
