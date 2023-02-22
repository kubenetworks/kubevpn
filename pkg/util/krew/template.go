package main

import (
	"bytes"
	"fmt"
	"path"
	"strings"
	"text/template"

	"github.com/sirupsen/logrus"
)

// InvalidPluginSpecError is invalid plugin spec error
type InvalidPluginSpecError struct {
	Spec string
	err  string
}

func (i InvalidPluginSpecError) Error() string {
	return i.err
}

// for backward compatibility
// by default addURIAndSha assumed 4 spaces indent
func fixShaIndentation(v string) string {
	return strings.Replace(v, "    sha256:", "sha256:", -1)
}

func indent(spaces int, v string) string {
	v = fixShaIndentation(v)
	pad := strings.Repeat(" ", spaces)
	return strings.TrimSpace(pad + strings.Replace(v, "\n", "\n"+pad, -1))
}

// ProcessTemplate process the .krew.yaml template for the release request
func ProcessTemplate(templateFile string, values interface{}, sha256Map map[string]string) ([]byte, error) {
	spec, err := RenderTemplate(templateFile, values, sha256Map)
	if err != nil {
		return nil, err
	}
	return spec, nil
}

// RenderTemplate process the .krew.yaml template for the release request
func RenderTemplate(templateFile string, values interface{}, sha256Map map[string]string) ([]byte, error) {
	logrus.Debugf("started processing of template %s", templateFile)
	name := path.Base(templateFile)
	t := template.New(name).Funcs(map[string]interface{}{
		"indent": indent,
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

			logrus.Infof("getting sha256 for %s", buf.String())
			sha256, ok := sha256Map[buf.String()]
			if !ok {
				panic(fmt.Errorf("can not get sha256 for link %s", buf.String()))
			}

			return fmt.Sprintf(`uri: %s
    sha256: %s`, buf.String(), sha256)
		},
	})

	templateObject, err := t.ParseFiles(templateFile)
	if err != nil {
		return nil, err
	}

	buf := new(bytes.Buffer)
	err = templateObject.Execute(buf, values)
	if err != nil {
		return nil, err
	}

	logrus.Debugf("completed processing of template")
	return buf.Bytes(), nil
}
