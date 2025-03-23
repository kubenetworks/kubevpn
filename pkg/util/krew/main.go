package main

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"path"
	"strings"
	"sync"
	"text/template"

	log "github.com/sirupsen/logrus"
)

func main() {
	// git describe --tags `git rev-list --tags --max-count=1`
	commitId, err2 := exec.Command("git", "rev-list", "--tags", "--max-count=1").Output()
	if err2 != nil {
		panic(err2)
	}
	tag, err2 := exec.Command("git", "describe", "--tags", strings.TrimSpace(string(commitId))).Output()
	if err2 != nil {
		panic(err2)
	}
	fmt.Printf("latest tag is %s", strings.TrimSpace(string(tag)))
	tplFile := ".github/krew.yaml"
	dstFile := "plugins/kubevpn.yaml"
	value := map[string]string{"TagName": strings.TrimSpace(string(tag))}

	name := path.Base(tplFile)
	var links []string
	t := template.New(name).Funcs(map[string]interface{}{
		"addURIAndSha": func(url, tag string) string {
			t := struct {
				TagName string
			}{
				TagName: tag,
			}
			buf := new(bytes.Buffer)
			temp, err := template.New("url").Parse(url)
			if err != nil {
				panic(err)
			}

			err = temp.Execute(buf, t)
			if err != nil {
				panic(err)
			}

			links = append(links, buf.String())
			return buf.String()
		},
	})

	templateObject, err := t.ParseFiles(tplFile)
	if err != nil {
		panic(err)
	}
	buf := new(bytes.Buffer)
	err = templateObject.Execute(buf, value)
	if err != nil {
		panic(err)
	}

	sha256Map := map[string]string{}
	wg := &sync.WaitGroup{}
	for i, link := range links {
		wg.Add(1)
		go func(i int, link string) {
			defer wg.Done()
			for k := 0; k < 10; k++ {
				asset, err2 := GetSha256ForAsset(link)
				if err2 != nil {
					plog.G(ctx).Error(err2)
					continue
				}
				sha256Map[link] = asset
				break
			}
		}(i, link)
	}
	wg.Wait()
	processTemplate, err := ProcessTemplate(tplFile, value, sha256Map)
	if err != nil {
		panic(err)
	}
	println(string(processTemplate))
	err = os.WriteFile(dstFile, processTemplate, 0644)
	if err != nil {
		panic(err)
	}

}
