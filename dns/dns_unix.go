// +build !windows

package dns

import (
	log "github.com/sirupsen/logrus"
	"io/fs"
	"io/ioutil"
	"os"
	"path/filepath"
)

func DNS(ip string, namespace string) error {
	var err error
	if err = os.MkdirAll(filepath.Join("/", "etc", "resolver"), fs.ModePerm); err != nil {
		log.Error(err)
	}
	filename := filepath.Join("/", "etc", "resolver", "local")
	fileContent := "nameserver " + ip
	_ = ioutil.WriteFile(filename, []byte(fileContent), fs.ModePerm)

	filename = filepath.Join("/", "etc", "resolver", namespace)
	fileContent = "nameserver " + ip + "\nsearch " + namespace + ".svc.cluster.local svc.cluster.local cluster.local\noptions ndots:5"
	_ = ioutil.WriteFile(filename, []byte(fileContent), fs.ModePerm)

	return nil
}
