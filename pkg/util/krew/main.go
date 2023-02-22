package main

import (
	"bytes"
	"os"
	"path"
	"sync"
	"text/template"

	log "github.com/sirupsen/logrus"
)

func main() {
	filePath := "../../../.github/krew.yaml"
	value := map[string]string{"TagName": "v1.1.20"}

	name := path.Base(filePath)
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

	templateObject, err := t.ParseFiles(filePath)
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
					log.Error(err2)
					continue
				}
				sha256Map[link] = asset
				break
			}
		}(i, link)
	}
	wg.Wait()
	processTemplate, err := ProcessTemplate(filePath, value, sha256Map)
	if err != nil {
		panic(err)
	}
	println(string(processTemplate))
	err = os.WriteFile("../../../plugins/kubevpn.yaml", processTemplate, 0644)
	if err != nil {
		panic(err)
	}

}
